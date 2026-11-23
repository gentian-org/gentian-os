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
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

func liaisonProfileForTest() *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: liaisonProfile}}
}

// signInBridgeProfile is a bridge from a catalog whose outside way names an
// authorization server and which asks to reach the tool server by host.
func signInBridgeProfile(name, issuer string) *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{
			profilebundle.OriginAnnotation: "cluster/shared",
			profilebundle.Annotation:       "bundle",
		}},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Bridge: &gentianov1alpha1.BridgeSpec{Ways: []gentianov1alpha1.BridgeWay{{
				Name: "tools",
				Outside: &gentianov1alpha1.OutsideToolServer{
					SignIn: &gentianov1alpha1.OutsideSignIn{OAuth: &gentianov1alpha1.OutsideOAuth{Issuer: issuer}},
				},
			}}},
			Requires: &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
				Egress: []gentianov1alpha1.EgressRequest{{Name: "tools", Reason: "the tool server", Hosts: []string{"mcp.example.org"}}},
			}},
		},
	}
}

func signInComponent(ns, name, profile string, granted ...string) *gentianov1alpha1.Component {
	c := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       gentianov1alpha1.ComponentSpec{ProfileRef: gentianov1alpha1.ProfileRef{Name: profile}},
	}
	for _, g := range granted {
		c.Spec.Privileges = append(c.Spec.Privileges, gentianov1alpha1.PrivilegeGrant{
			Privilege: g, Approver: "ann", ApprovedAt: metav1.Now(),
		})
	}
	return c
}

func signInClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gentianov1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// The liaison reaches where the people of a tenant's bridge sign in once the
// bridge's own egress is granted: no person names the host for the liaison.
func TestTheLiaisonReachesTheSignInHostsOfItsTenantsGrantedBridges(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	liaison := signInComponent("tenant-acme", "liaison", liaisonProfile)
	c := signInClient(t,
		liaisonProfileForTest(),
		signInBridgeProfile("plaud", "https://mcp.plaud.ai"),
		signInBridgeProfile("waiting", "https://auth.waiting.example"),
		signInBridgeProfile("elsewhere", "https://auth.elsewhere.example"),
		liaison,
		signInComponent("tenant-acme", "plaud", "plaud", "egress/tools"),
		signInComponent("tenant-acme", "waiting", "waiting"),
		signInComponent("tenant-other", "elsewhere", "elsewhere", "egress/tools"),
	)
	got, err := bridgeHosts(context.Background(), c, liaisonProfileForTest(), liaison, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"mcp.plaud.ai"}) {
		t.Fatalf("the liaison reaches %v, want only the granted bridge's sign-in host mcp.plaud.ai", got)
	}

	// Any other component, and a liaison a catalog brought, reach none.
	other := signInBridgeProfile("plaud", "https://mcp.plaud.ai")
	if got, _ := bridgeHosts(context.Background(), c, other, signInComponent("tenant-acme", "plaud", "plaud"), tenant); got != nil {
		t.Fatalf("a bridge reaches %v through the liaison's rule", got)
	}
	brought := liaisonProfileForTest()
	brought.Annotations = map[string]string{profilebundle.OriginAnnotation: "cluster/shared", profilebundle.Annotation: "bundle"}
	if got, _ := bridgeHosts(context.Background(), c, brought, liaison, tenant); got != nil {
		t.Fatalf("a liaison a catalog brought reaches %v", got)
	}
}

// A bridge that changes wakes the liaison of its tenant; a component that
// names no sign-in does not.
func TestABridgeWakesTheLiaisonOfItsTenant(t *testing.T) {
	plain := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "notes"}}
	c := signInClient(t,
		liaisonProfileForTest(), plain,
		signInBridgeProfile("plaud", "https://mcp.plaud.ai"),
		signInComponent("tenant-acme", "liaison", liaisonProfile),
		signInComponent("tenant-other", "liaison", liaisonProfile),
	)
	ctx := context.Background()
	got := liaisonsWokenBy(ctx, c, signInComponent("tenant-acme", "plaud", "plaud"))
	want := []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: "tenant-acme", Name: "liaison"}}}
	if !slices.Equal(got, want) {
		t.Fatalf("a bridge woke %v, want %v", got, want)
	}
	if got := liaisonsWokenBy(ctx, c, signInComponent("tenant-acme", "notes", "notes")); len(got) != 0 {
		t.Fatalf("a component without a sign-in woke %v", got)
	}
}

// outsideAgentBridge is an agent bridge to a tool server outside that runs
// nothing of its own, asking to reach host.
func outsideAgentBridge(name, host string, runsPods bool) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{
			profilebundle.OriginAnnotation: "cluster/shared",
			profilebundle.Annotation:       "bundle",
		}},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassBridge},
			Bridge: &gentianov1alpha1.BridgeSpec{Ways: []gentianov1alpha1.BridgeWay{{
				Name: "tools", Caller: gentianov1alpha1.BridgeCallerAgents,
			}}},
			Requires: &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
				Egress: []gentianov1alpha1.EgressRequest{{Name: "tools", Reason: "the tool server", Hosts: []string{host}}},
			}},
		},
	}
	if !runsPods {
		p.Spec.Package.API = &gentianov1alpha1.APIIntegration{}
	}
	return p
}

// vouchingCaller is a component that may speak as a person: the caller of
// the tenant's agent bridges.
func vouchingCaller(name string) *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Vouching: &gentianov1alpha1.VouchingRequirement{},
			}},
		},
	}
}

// The caller of the tenant's agent bridges reaches the hosts granted to the
// bridges that run nothing of their own, and no others: not an ungranted
// bridge's, not one whose pods make their own calls, not another tenant's.
func TestTheCallerReachesTheHostsOfTheOutsideBridgesItCalls(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	chaperone := signInComponent("tenant-acme", "chaperone", "chaperone")
	c := signInClient(t,
		vouchingCaller("chaperone"),
		outsideAgentBridge("plaud", "mcp.plaud.ai", false),
		outsideAgentBridge("waiting", "mcp.waiting.example", false),
		outsideAgentBridge("runs-pods", "api.runs-pods.example", true),
		outsideAgentBridge("elsewhere", "mcp.elsewhere.example", false),
		chaperone,
		signInComponent("tenant-acme", "plaud", "plaud", "egress/tools"),
		signInComponent("tenant-acme", "waiting", "waiting"),
		signInComponent("tenant-acme", "runs-pods", "runs-pods", "egress/tools"),
		signInComponent("tenant-other", "elsewhere", "elsewhere", "egress/tools"),
	)
	got, err := bridgeHosts(context.Background(), c, vouchingCaller("chaperone"), chaperone, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"mcp.plaud.ai"}) {
		t.Fatalf("the caller reaches %v, want only the granted outside bridge's host mcp.plaud.ai", got)
	}

	// A change to an outside bridge wakes the tenant's callers.
	woken := liaisonsWokenBy(context.Background(), c, signInComponent("tenant-acme", "plaud", "plaud"))
	want := []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: "tenant-acme", Name: "chaperone"}}}
	if !slices.Equal(woken, want) {
		t.Fatalf("an outside bridge woke %v, want %v", woken, want)
	}
	// A bridge that runs its own pods wakes nobody: its calls are its own.
	if woken := liaisonsWokenBy(context.Background(), c, signInComponent("tenant-acme", "runs-pods", "runs-pods")); len(woken) != 0 {
		t.Fatalf("a bridge with pods woke %v", woken)
	}
}
