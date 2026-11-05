/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A retired plan is kept for the tenants on it and offered to no other.
func TestARetiredPlanIsOfferedOnlyToTheTenantOnIt(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cpu := func(v string) gentianov1alpha1.TenantQuotas {
		q := resource.MustParse(v)
		return gentianov1alpha1.TenantQuotas{RequestsCPU: &q}
	}
	oldQuotas := cpu("4")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&gentianov1alpha1.ResourcePlan{ObjectMeta: metav1.ObjectMeta{Name: "old"},
			Spec: gentianov1alpha1.ResourcePlanSpec{DisplayName: "Old", Tier: 0, Units: 1, Retired: true, Quotas: cpu("4")}},
		&gentianov1alpha1.ResourcePlan{ObjectMeta: metav1.ObjectMeta{Name: "new"},
			Spec: gentianov1alpha1.ResourcePlanSpec{DisplayName: "New", Tier: 1, Units: 1, Default: true, Quotas: cpu("5")}},
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "on-old"}, Spec: gentianov1alpha1.TenantSpec{Quotas: &oldQuotas}},
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere"}},
	).Build()
	s := &Service{client: c}

	for tenant, wantSelectable := range map[string]bool{"on-old": true, "elsewhere": false} {
		plans, err := s.Plans(context.Background(), tenant, false)
		if err != nil {
			t.Fatal(err)
		}
		old := plans[0]
		if old.Name != "old" || !old.Retired || old.Units != 1 {
			t.Fatalf("%s: old = %+v", tenant, old)
		}
		if old.Selectable != wantSelectable {
			t.Errorf("%s: retired plan selectable = %v, want %v", tenant, old.Selectable, wantSelectable)
		}
		if !wantSelectable && old.BlockedBy != PlanBlockedRetired {
			t.Errorf("%s: blockedBy = %q", tenant, old.BlockedBy)
		}
	}
}
