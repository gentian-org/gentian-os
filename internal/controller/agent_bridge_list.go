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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

// The list of a tenant's agent bridges, told to the components that may call
// them. A caller is the component that checks every tool call made for a
// person (requires.services.vouching); the agent bridges admit it and
// nothing else (netpolicy/agent_bridge.go). What it cannot know by itself is
// which agent bridges its tenant has: so each caller is given a ConfigMap,
// bridges-<component>, whose one key is the list as JSON, from a bridge's
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
	// bridgeListLabel marks the ConfigMaps written here, for the sweep.
	bridgeListLabel = "gentianos.io/bridge-list"
	// BridgeListKey is the ConfigMap's one key.
	BridgeListKey = "BRIDGES"
)

// BridgeListName is the ConfigMap a caller's list is in.
func BridgeListName(component string) string { return "bridges-" + component }

// bridgeListEntry is one agent bridge as a caller is told of it.
type bridgeListEntry struct {
	URL string `json:"url"`
	App string `json:"app"`
}

// bridgeList is the list for a tenant: its installed agent bridges that say
// where they answer. JSON with sorted keys, so the same bridges are the
// same bytes.
func bridgeList(namespace string, bridges []string, profiles map[string]*gentianov1alpha1.ComponentProfile) (string, error) {
	list := map[string]bridgeListEntry{}
	for _, name := range bridges {
		profile := profiles[name]
		if profile == nil {
			continue
		}
		app := agentBridgeApp(name, profile)
		if app == "" {
			continue
		}
		list[name] = bridgeListEntry{URL: contractURL(profile.AgentWay().Endpoint, namespace), App: app}
	}
	raw, err := json.Marshal(list)
	return string(raw), err
}

// ensureBridgeLists writes the list for every caller of the tenant and
// removes the list of a component that is a caller no more.
func (r *TenantReconciler) ensureBridgeLists(ctx context.Context, tenant *gentianov1alpha1.Tenant, in netpolicy.BuildInput) error {
	bridges, callers := netpolicy.AgentBridgeRoles(in)
	list, err := bridgeList(in.Namespace, bridges, in.Profiles)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, caller := range callers {
		name := BridgeListName(caller)
		wanted[name] = true
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: in.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			if cm.Labels == nil {
				cm.Labels = map[string]string{}
			}
			cm.Labels[bridgeListLabel] = "true"
			cm.Data = map[string]string{BridgeListKey: list}
			return controllerutil.SetControllerReference(tenant, cm, r.Scheme)
		}); err != nil {
			return fmt.Errorf("write the bridge list %s: %w", name, err)
		}
	}
	existing := &corev1.ConfigMapList{}
	if err := r.List(ctx, existing, client.InNamespace(in.Namespace), client.MatchingLabels{bridgeListLabel: "true"}); err != nil {
		return err
	}
	for i := range existing.Items {
		if wanted[existing.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &existing.Items[i]); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("remove the bridge list %s: %w", existing.Items[i].Name, err)
		}
	}
	return nil
}
