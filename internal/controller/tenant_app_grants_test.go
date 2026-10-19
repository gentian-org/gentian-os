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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The tenant's grants are the AppGrants of its namespace: each made with who
// granted it and when, one withdrawn from the Tenant removed, and an AppGrant
// nobody declared here left alone.
func TestATenantsGrantsAreTheAppGrantsOfItsNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := appStoreScheme(t)
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "team-uid"}}
	at := metav1.NewTime(time.Date(2026, 10, 19, 8, 37, 0, 0, time.UTC))
	tenant.Spec.Grants = []gentianov1alpha1.TenantAppGrant{
		{
			AppGrantSpec: gentianov1alpha1.AppGrantSpec{App: "aluvian-statistics", Consume: []gentianov1alpha1.ConsumeGrantSpec{
				{Contract: "aluvian-claims", Granted: []string{"claims:read"}},
			}},
			GrantedBy: "1678d05a", GrantedAt: &at, Reason: "the statistics read who claimed which tenant",
		},
		{AppGrantSpec: gentianov1alpha1.AppGrantSpec{App: "carried-over"}},
	}
	someoneElses := &gentianov1alpha1.AppGrant{ObjectMeta: metav1.ObjectMeta{Name: "by-hand", Namespace: "tenant-team"},
		Spec: gentianov1alpha1.AppGrantSpec{App: "by-hand"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), someoneElses).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}
	if err := r.ensureAppGrants(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	ag := &gentianov1alpha1.AppGrant{}
	if err := c.Get(ctx, types.NamespacedName{Name: "aluvian-statistics", Namespace: "tenant-team"}, ag); err != nil {
		t.Fatal(err)
	}
	if ag.Spec.App != "aluvian-statistics" || len(ag.Spec.Consume) != 1 || ag.Spec.Consume[0].Granted[0] != "claims:read" {
		t.Fatalf("spec = %+v", ag.Spec)
	}
	if ag.Annotations[appGrantGrantedByAnnotation] != "1678d05a" || ag.Annotations[appGrantGrantedAtAnnotation] != "2026-10-19T08:37:00Z" ||
		ag.Annotations[appGrantReasonAnnotation] == "" {
		t.Fatalf("annotations = %v", ag.Annotations)
	}
	if ag.Labels[appGrantDeclaredLabel] != appGrantDeclaredValue || !metav1.IsControlledBy(ag, tenant) {
		t.Fatalf("not marked as the tenant's: labels %v, owners %v", ag.Labels, ag.OwnerReferences)
	}
	carried := &gentianov1alpha1.AppGrant{}
	if err := c.Get(ctx, types.NamespacedName{Name: "carried-over", Namespace: "tenant-team"}, carried); err != nil {
		t.Fatal(err)
	}
	if _, has := carried.Annotations[appGrantGrantedByAnnotation]; has {
		t.Fatalf("a grant without a granter got one: %v", carried.Annotations)
	}

	// Withdrawn from the Tenant: its AppGrant goes, the hand-made one stays.
	tenant.Spec.Grants = tenant.Spec.Grants[1:]
	if err := r.ensureAppGrants(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "aluvian-statistics", Namespace: "tenant-team"}, ag); !errors.IsNotFound(err) {
		t.Fatalf("a withdrawn grant is still there: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "by-hand", Namespace: "tenant-team"}, &gentianov1alpha1.AppGrant{}); err != nil {
		t.Fatalf("an AppGrant nobody declared here was removed: %v", err)
	}
}
