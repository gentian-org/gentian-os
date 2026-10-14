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
)

// An app the Composition installs gets the outbound rules a person granted
// on it, in a policy of their own that selects its pods by the release the
// Composition names. Without a grant there is no such policy; revoking the
// grant takes it away again.
func TestAGrantReachesAnAppTheCompositionInstalls(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	profile := profileRequestingEgress("shop")
	profile.Spec.Package.Chart = &gentianov1alpha1.ChartRef{Repository: "oci://example/charts", Name: "shop", Version: "1.0.0"}
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		OIDC: &gentianov1alpha1.OIDCValueMapping{IssuerKey: "oidc.issuer"},
	}
	if !composedDelivery(profile) {
		t.Fatal("the fixture is meant to be an app the Composition installs")
	}
	comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "tenant-acme"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	key := types.NamespacedName{Name: grantedEgressPolicyName(comp), Namespace: comp.Namespace}

	// Asked for and not granted: nothing is opened.
	if err := r.ensureGrantedEgress(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &networkingv1.NetworkPolicy{}); err == nil {
		t.Fatal("a policy was written for a request nobody granted")
	}

	comp.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{{
		Privilege: "egress/smtp-relay", Approver: "u-tom", ApprovedAt: metav1.Now(), Reason: "agreed in the review",
	}}
	if err := r.ensureGrantedEgress(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, key, np); err != nil {
		t.Fatalf("no policy after the grant: %v", err)
	}
	if got := np.Spec.PodSelector.MatchLabels[componentInstanceLabel]; got != "shop-release" {
		t.Fatalf("the policy selects %q, want the Composition's release", got)
	}
	if len(np.Spec.Egress) != 1 || np.Spec.Egress[0].To[0].IPBlock == nil || np.Spec.Egress[0].To[0].IPBlock.CIDR != "203.0.113.0/24" {
		t.Fatalf("the policy holds %+v, want the granted rule and nothing else", np.Spec.Egress)
	}

	comp.Spec.Privileges = nil
	if err := r.ensureGrantedEgress(ctx, comp, profile); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, &networkingv1.NetworkPolicy{}); err == nil {
		t.Fatal("the policy outlived the grant")
	}
}
