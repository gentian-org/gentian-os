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
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An agent bridge that is a tool server outside the cluster runs nothing in
// the tenant: the egress the tenant granted it is the way out of the
// component that calls it (#1286). What the Tenant granted reaches that
// caller's policy, and builds no policy of its own for an app.
func TestTheTenantsGrantForAnOutsideBridgeOpensTheWayOutOfItsCaller(t *testing.T) {
	ctx := context.Background()
	scheme := appStoreScheme(t)

	bridge := profileFixture("tools", false, gentianov1alpha1.ComponentClassBridge)
	bridge.Spec.Launch = gentianov1alpha1.ComponentLaunchNone
	bridge.Spec.Package.API = &gentianov1alpha1.APIIntegration{BaseURL: "https://tools.example/mcp"}
	bridge.Spec.Bridge = &gentianov1alpha1.BridgeSpec{Ways: []gentianov1alpha1.BridgeWay{{
		Name: "tools", Caller: gentianov1alpha1.BridgeCallerAgents, Callee: gentianov1alpha1.BridgeCalleeOutside,
		Protocol: gentianov1alpha1.BridgeProtocolMCP, SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson,
	}}}
	bridge.Spec.Requires = &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
		Egress: []gentianov1alpha1.EgressRequest{{
			Name:   "tool-server",
			Rule:   &networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.10/32"}}}},
			Reason: "the tool server this bridge offers",
		}},
	}}
	caller := chartProfileFixture("oci://registry.example/apps")
	caller.Name = "chaperone"
	caller.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Vouching: &gentianov1alpha1.VouchingRequirement{},
	}}

	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "uid-acme"}}
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "tools"}, {Profile: "chaperone"}}
	ns := tenantNamespaceName(tenant)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), bridge, caller).Build()
	tr := &TenantReconciler{Client: c, Scheme: scheme}

	callersPolicy := func() *networkingv1.NetworkPolicy {
		t.Helper()
		if err := tr.ensureNetworkPolicies(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		np := &networkingv1.NetworkPolicy{}
		if err := c.Get(ctx, types.NamespacedName{Name: "agent-bridge-callers-chaperone", Namespace: ns}, np); err != nil {
			return nil
		}
		return np
	}

	if np := callersPolicy(); np != nil {
		t.Fatalf("egress nobody granted reached the caller: %+v", np.Spec)
	}

	tenant.Spec.Privileges = []gentianov1alpha1.TenantPrivilegeGrant{{
		Install: "tools", Privilege: "egress/tool-server", Approver: "perimeter-approver",
		ApprovedAt: metav1.Now(), Reason: "the team uses these tools",
	}}
	np := callersPolicy()
	if np == nil || len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].To) != 1 ||
		np.Spec.Egress[0].To[0].IPBlock == nil || np.Spec.Egress[0].To[0].IPBlock.CIDR != "203.0.113.10/32" {
		t.Fatalf("the caller must reach the granted tool server, got %+v", np)
	}

	list := &networkingv1.NetworkPolicyList{}
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	for _, p := range list.Items {
		if strings.HasPrefix(p.Name, "app-egress-") {
			t.Errorf("%s was built from the tenant's grants", p.Name)
		}
	}

	// Withdrawn: the way out goes with it.
	tenant.Spec.Privileges = nil
	if np := callersPolicy(); np != nil {
		t.Fatalf("the way out outlived the grant: %+v", np.Spec)
	}
}
