/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/security"
)

// The list of a tenant's agent bridges, told to the components that may call
// them. A caller is the component that checks every tool call made for a
// person (requires.services.vouching); the agent bridges admit it and
// nothing else (netpolicy/agent_bridge.go). What it cannot know by itself is
// which agent bridges its tenant has: so each caller is given a ConfigMap,
// agent-bridges-<component>, whose one key is the list as JSON, from a bridge's
// install name to its address inside the tenant and the app a call through
// it is for (agentBridgeApp): the app it connects, or the bridge itself where
// it connects none:
//
//	{"odoo-adapter-ee": {"url": "<its Service, port and path>", "app": "odoo"}}
//
// It follows installs: an agent bridge is on it while it is installed and
// says where it answers (the endpoint of its way from the agents). It holds
// no credential and grants nothing: the way to a bridge is the network
// policy's, and what a call may do is the app's own answer to the credential
// the call carries.
const (
	// agentBridgeListLabel marks the ConfigMaps written here, for the sweep.
	agentBridgeListLabel = "gentianos.io/agent-bridge-list"
	// AgentBridgeListKey is the ConfigMap's one key.
	AgentBridgeListKey = "AGENT_BRIDGES"
)

// AgentBridgeListName is the ConfigMap a caller's list is in.
func AgentBridgeListName(component string) string { return "agent-bridges-" + component }

// agentBridgeListEntry is one agent bridge as a caller is told of it.
type agentBridgeListEntry struct {
	// URL is where the tool server answers.
	URL string `json:"url"`
	// App is the app a person's right is asked about: the app the bridge
	// connects, or the bridge itself.
	App string `json:"app"`
	// Outside is what the entry says of a tool server outside the platform
	// (spec.bridge.ways[].outside): the tools that may be called and where
	// a person signs in. Absent for a bridge inside the tenant, which is
	// asked for its tools.
	Outside *gentianov1alpha1.OutsideToolServer `json:"outside,omitempty"`
}

// agentBridgeList is the tenant's agent bridges as their callers are told:
// by name, each with the address of its tool server and the app a person's
// right is asked about. A bridge inside the tenant is listed by its way's
// endpoint. A bridge to an outside tool server is listed by the entry's
// baseUrl, with what the entry says of the server.
func agentBridgeList(namespace string, bridges []string, profiles map[string]*gentianov1alpha1.ComponentProfile) (string, error) {
	list := map[string]agentBridgeListEntry{}
	for _, name := range bridges {
		profile := profiles[name]
		if profile == nil {
			continue
		}
		if outside := outsideToolServer(profile); outside != nil {
			list[name] = agentBridgeListEntry{URL: profile.Spec.Package.API.BaseURL, App: name, Outside: outside}
			continue
		}
		app := agentBridgeApp(name, profile)
		if app == "" {
			continue
		}
		list[name] = agentBridgeListEntry{URL: contractURL(profile.AgentWay().Endpoint, namespace), App: app}
	}
	raw, err := json.Marshal(list)
	return string(raw), err
}

// answeredBridges are bridges without the outside tool servers that still
// wait for an answer to something they ask for. A bridge inside the tenant
// is held by its own install and answers nobody until it runs. A bridge to
// an outside server runs nothing of its own: being on the list is what makes
// its callers, and whoever signs people in for them, reach the server. So it
// is listed once what its entry asks for was granted or is answered by
// policy, and not before.
func answeredBridges(
	ctx context.Context, c client.Client, tenant *gentianov1alpha1.Tenant,
	bridges []string, profiles map[string]*gentianov1alpha1.ComponentProfile,
) ([]string, error) {
	out := make([]string, 0, len(bridges))
	for _, name := range bridges {
		profile := profiles[name]
		if profile == nil || outsideToolServer(profile) == nil {
			out = append(out, name)
			continue
		}
		answered, err := answeredEgress(ctx, c, profile, tenant)
		if err != nil {
			return nil, err
		}
		asked := &gentianov1alpha1.Component{Spec: gentianov1alpha1.ComponentSpec{Privileges: security.TenantGrants(tenant, name)}}
		if len(security.PendingPrivilegesUnder(profile, asked, time.Now(), answered)) == 0 {
			out = append(out, name)
		}
	}
	return out, nil
}

// outsideToolServer is what a bridge's entry says of an outside tool server,
// or nil where it has none or names no address for it. The schema asks for
// an https address; it is asked again here, because the list is what makes
// a caller send a person's credential there.
func outsideToolServer(profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.OutsideToolServer {
	way := profile.AgentWay()
	if way == nil || way.Outside == nil || way.Callee != gentianov1alpha1.BridgeCalleeOutside {
		return nil
	}
	api := profile.Spec.Package.API
	if api == nil || !strings.HasPrefix(api.BaseURL, "https://") {
		return nil
	}
	return way.Outside
}

func (r *TenantReconciler) ensureAgentBridgeLists(ctx context.Context, tenant *gentianov1alpha1.Tenant, in netpolicy.BuildInput) error {
	bridges, callers := netpolicy.AgentBridgeRoles(in)
	bridges, err := answeredBridges(ctx, r.Client, tenant, bridges, in.Profiles)
	if err != nil {
		return err
	}
	list, err := agentBridgeList(in.Namespace, bridges, in.Profiles)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, caller := range callers {
		name := AgentBridgeListName(caller)
		wanted[name] = true
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: in.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			if cm.Labels == nil {
				cm.Labels = map[string]string{}
			}
			cm.Labels[agentBridgeListLabel] = "true"
			cm.Data = map[string]string{AgentBridgeListKey: list}
			return controllerutil.SetControllerReference(tenant, cm, r.Scheme)
		}); err != nil {
			return fmt.Errorf("write the agent bridge list %s: %w", name, err)
		}
	}
	existing := &corev1.ConfigMapList{}
	if err := r.List(ctx, existing, client.InNamespace(in.Namespace), client.MatchingLabels{agentBridgeListLabel: "true"}); err != nil {
		return err
	}
	for i := range existing.Items {
		if wanted[existing.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &existing.Items[i]); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("remove the agent bridge list %s: %w", existing.Items[i].Name, err)
		}
	}
	return nil
}
