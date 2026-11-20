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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

func bridgeListProfile(classes []gentianov1alpha1.ComponentClass, mutate func(*gentianov1alpha1.ComponentProfile)) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{Classes: classes}}
	if mutate != nil {
		mutate(p)
	}
	return p
}

// Every component that may call the tenant's agent bridges is told which
// there are: the installed agent bridges that say where they answer, each
// with its address inside the tenant and the app a call through it is for:
// the app it connects, or the bridge itself where it connects none. A caller the platform
// places is told like an installed one; a component that may not call them
// is told nothing; and the list follows what is installed, down to the list
// of a component that is a caller no more.
func TestEveryCallerIsToldTheTenantsAgentBridges(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	app := []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}
	bridge := []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassBridge}
	agentWay := func(endpoint *gentianov1alpha1.ContractEndpoint) []gentianov1alpha1.BridgeWay {
		return []gentianov1alpha1.BridgeWay{{Name: "tools", Caller: gentianov1alpha1.BridgeCallerAgents,
			Callee: gentianov1alpha1.BridgeCalleeApp, Protocol: gentianov1alpha1.BridgeProtocolMCP,
			SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson, Endpoint: endpoint}}
	}
	speaks := func(p *gentianov1alpha1.ComponentProfile) {
		p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
			Vouching: &gentianov1alpha1.VouchingRequirement{},
		}}
	}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"odoo":      bridgeListProfile(app, nil),
		"chaperone": bridgeListProfile(app, speaks),
		"usher":     bridgeListProfile(app, speaks),
		"desktop":   bridgeListProfile(app, nil),
		"odoo-adapter": bridgeListProfile(bridge, func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Bridge = &gentianov1alpha1.BridgeSpec{
				App:  &gentianov1alpha1.BridgedApp{Profile: "odoo"},
				Ways: agentWay(&gentianov1alpha1.ContractEndpoint{Service: "odoo-adapter", Port: 8765, Path: "/mcp"}),
			}
		}),
		// Connects none of the tenant's apps and reaches a service elsewhere:
		// a call through it is for the bridge itself.
		"store-adapter": bridgeListProfile(bridge, func(p *gentianov1alpha1.ComponentProfile) {
			ways := agentWay(&gentianov1alpha1.ContractEndpoint{Service: "store-adapter", Port: 8765, Path: "/mcp"})
			ways[0].Callee = gentianov1alpha1.BridgeCalleePartner
			p.Spec.Bridge = &gentianov1alpha1.BridgeSpec{Ways: ways}
		}),
		// Installed, and silent about where it answers: on nobody's list.
		"silent-adapter": bridgeListProfile(bridge, func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Bridge = &gentianov1alpha1.BridgeSpec{App: &gentianov1alpha1.BridgedApp{Profile: "odoo"}, Ways: agentWay(nil)}
		}),
		// A sync bridge has no way from the agents: on nobody's list.
		"sync": bridgeListProfile(bridge, func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Bridge = &gentianov1alpha1.BridgeSpec{App: &gentianov1alpha1.BridgedApp{Profile: "odoo"},
				Ways: []gentianov1alpha1.BridgeWay{{Name: "files", Caller: gentianov1alpha1.BridgeCallerDevice,
					Callee: gentianov1alpha1.BridgeCalleeApp, Protocol: gentianov1alpha1.BridgeProtocolWebDAV,
					SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson, Paths: []string{"/dav"}}}}
		}),
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "uid-acme"}}
	in := netpolicy.BuildInput{
		TenantName: "acme", Namespace: "tenant-acme", Profiles: profiles,
		Apps: []gentianov1alpha1.TenantApp{
			{Profile: "odoo"}, {Profile: "chaperone"}, {Profile: "odoo-adapter"}, {Profile: "silent-adapter"},
			{Profile: "store-adapter"}, {Profile: "sync"},
		},
		Placed: []string{"usher", "desktop"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy()).Build()
	tr := &TenantReconciler{Client: c, Scheme: scheme}

	list := func(component string) (string, bool) {
		t.Helper()
		cm := &corev1.ConfigMap{}
		err := c.Get(ctx, types.NamespacedName{Namespace: "tenant-acme", Name: AgentBridgeListName(component)}, cm)
		if errors.IsNotFound(err) {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != "acme" {
			t.Fatalf("%s is owned by %+v, want the tenant", cm.Name, cm.OwnerReferences)
		}
		return cm.Data[AgentBridgeListKey], true
	}

	if err := tr.ensureAgentBridgeLists(ctx, tenant, in); err != nil {
		t.Fatal(err)
	}
	const want = `{"odoo-adapter":{"url":"http://odoo-adapter.tenant-acme.svc.cluster.local:8765/mcp","app":"odoo"},` +
		`"store-adapter":{"url":"http://store-adapter.tenant-acme.svc.cluster.local:8765/mcp","app":"store-adapter"}}`
	for _, caller := range []string{"chaperone", "usher"} {
		if got, ok := list(caller); !ok || got != want {
			t.Fatalf("%s is told %q (present %v), want %s", caller, got, ok, want)
		}
	}
	for _, other := range []string{"desktop", "odoo", "odoo-adapter"} {
		if _, ok := list(other); ok {
			t.Fatalf("%s may not call agent bridges and was given a list", other)
		}
	}

	// The bridges are uninstalled and the placed caller withdrawn: the one
	// caller left is told there is none, and the other's list is gone.
	in.Apps = []gentianov1alpha1.TenantApp{{Profile: "odoo"}, {Profile: "chaperone"}}
	in.Placed = []string{"desktop"}
	if err := tr.ensureAgentBridgeLists(ctx, tenant, in); err != nil {
		t.Fatal(err)
	}
	if got, ok := list("chaperone"); !ok || got != "{}" {
		t.Fatalf("chaperone is told %q (present %v), want an empty list", got, ok)
	}
	if _, ok := list("usher"); ok {
		t.Fatal("a component that is a caller no more kept its list")
	}
}

// A bridge to an outside tool server is listed by the entry's address, with
// the tools the entry names and where a person signs in, and with the bridge
// itself as the app a person's right is asked about. One whose address is
// not https is on nobody's list.
func TestABridgeToAnOutsideToolServerIsListedWithWhatItsEntrySays(t *testing.T) {
	bridge := []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassBridge}
	outside := func(base string) *gentianov1alpha1.ComponentProfile {
		return bridgeListProfile(bridge, func(p *gentianov1alpha1.ComponentProfile) {
			p.Spec.Package.API = &gentianov1alpha1.APIIntegration{BaseURL: base}
			p.Spec.Bridge = &gentianov1alpha1.BridgeSpec{Ways: []gentianov1alpha1.BridgeWay{{
				Name: "tools", Caller: gentianov1alpha1.BridgeCallerAgents, Callee: gentianov1alpha1.BridgeCalleeOutside,
				Protocol: gentianov1alpha1.BridgeProtocolMCP, SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson,
				Outside: &gentianov1alpha1.OutsideToolServer{
					Tools: []gentianov1alpha1.OutsideTool{{
						Name: "list-files", Tool: "list_files", Version: 1, Class: "read", Description: "Lists recordings.",
						InputSchema: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"limit":{"type":"integer"}}}`)},
					}},
					SignIn: &gentianov1alpha1.OutsideSignIn{OAuth: &gentianov1alpha1.OutsideOAuth{Issuer: "https://tools.example"}},
				},
			}}}
		})
	}
	raw, err := agentBridgeList("tenant-acme", []string{"recorder", "plain"}, map[string]*gentianov1alpha1.ComponentProfile{
		"recorder": outside("https://tools.example/mcp"),
		"plain":    outside("http://tools.example/mcp"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"recorder":{"url":"https://tools.example/mcp","app":"recorder","outside":{"tools":[{"name":"list-files","tool":"list_files","version":1,"class":"read","description":"Lists recordings.","inputSchema":{"type":"object","properties":{"limit":{"type":"integer"}}}}],"signIn":{"oauth":{"issuer":"https://tools.example"}}}}}`
	if raw != want {
		t.Fatalf("the list:\n%s\nwant:\n%s", raw, want)
	}
}
