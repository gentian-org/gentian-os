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

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A partner app the Composition installs -- the store on Aluvian: it accepts
// partners and declares secrets, so its chart is the Composition's -- gets
// the component policy every other component gets, selecting its pods by the
// release the Composition names. Without it the app reached none of what
// this reconciler wires, the realm first: its token request timed out.
// Granted rules are in the same policy, the policy that once held them alone
// goes, and the database stays the Composition's to open.
func TestAComposedAppGetsItsComponentPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	profile := profileRequestingEgress("shop")
	profile.Spec.Package.Chart = &gentianov1alpha1.ChartRef{Repository: "oci://example/charts", Name: "shop", Version: "1.0.0"}
	profile.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{}
	profile.Spec.Requires.Services = &gentianov1alpha1.ServiceRequirements{
		Partners: &gentianov1alpha1.PartnersRequirement{Accept: true},
		Database: &gentianov1alpha1.DatabaseRequirement{},
	}
	if !composedDelivery(profile) || !wantsPartners(profile) {
		t.Fatal("the fixture is meant to be a partner app the Composition installs")
	}
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "tenant-acme"}}
	legacy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "granted-egress-shop", Namespace: "tenant-acme"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, legacy).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	tenant := acmeTenantFixture()

	comp.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{{
		Privilege: "egress/smtp-relay", Approver: "u-tom", ApprovedAt: metav1.Now(), Reason: "agreed in the review",
	}}
	if err := r.ensureNetworkPolicy(ctx, comp, profile, tenant, false); err != nil {
		t.Fatal(err)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "component-shop", Namespace: "tenant-acme"}, np); err != nil {
		t.Fatalf("no component policy for a composed app: %v", err)
	}
	if got := np.Spec.PodSelector.MatchLabels[componentInstanceLabel]; got != "shop-release" {
		t.Fatalf("the policy selects %q, want the Composition's release", got)
	}
	var realm, granted bool
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.NamespaceSelector != nil {
				switch to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] {
				case layout.Namespace(layout.Authentication):
					realm = true
				case r.componentDatabaseNamespace(tenant):
					t.Fatal("the database is the Composition's to open, on its port only")
				}
			}
			if to.IPBlock != nil && to.IPBlock.CIDR == "203.0.113.0/24" {
				granted = true
			}
		}
	}
	if !realm || !granted {
		t.Fatalf("realm %v, granted rule %v, in %+v", realm, granted, np.Spec.Egress)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "granted-egress-shop", Namespace: "tenant-acme"}, &networkingv1.NetworkPolicy{}); err == nil {
		t.Fatal("the policy that held the granted rules alone is still there")
	}
}
