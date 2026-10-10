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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The operator records a grant and touches no authorization store: the
// director is the store's only writer. A reconcile therefore needs nothing
// but the object, and leaves it Ready with its generation observed.
func TestAppGrantIsRecordedWithoutAStore(t *testing.T) {
	t.Parallel()
	grant := &gentianov1alpha1.AppGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "provider",
			Namespace:  "tenant-demo",
			Labels:     map[string]string{tenantLabel: "demo"},
			Finalizers: []string{appGrantFinalizer},
			Generation: 3,
		},
		Spec: gentianov1alpha1.AppGrantSpec{
			App:     "provider",
			Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "files", Granted: []string{"read"}}},
		},
	}
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(grant).WithStatusSubresource(grant).Build()
	r := &AppGrantReconciler{Client: c}
	key := types.NamespacedName{Name: grant.Name, Namespace: grant.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	updated := &gentianov1alpha1.AppGrant{}
	if err := c.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("get grant: %v", err)
	}
	if updated.Status.Phase != gentianov1alpha1.AppGrantPhaseReady {
		t.Fatalf("phase = %q, want Ready", updated.Status.Phase)
	}
	if updated.Status.ObservedGeneration != 3 {
		t.Fatalf("observedGeneration = %d, want 3", updated.Status.ObservedGeneration)
	}
}

// A tenant whose apps declare an integration is reconciled without any grant
// being written, and a grant an administrator narrowed is left as it is:
// what a profile declares is a request, and the reconciler answers none.
func TestIntegrationStageWritesNoGrantAndLeavesANarrowOneAlone(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	consumer := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Integrations: []gentianov1alpha1.IntegrationRef{{
				Contract: "files", Provider: "provider", Capabilities: []string{"read", "write"},
			}},
		},
	}
	provider := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "provider"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Provides: []gentianov1alpha1.ContractRef{{Name: "files"}},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantApp{{Profile: "consumer"}, {Profile: "provider"}},
		},
	}
	narrow := &gentianov1alpha1.AppGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: tenantNamespaceName(tenant)},
		Spec: gentianov1alpha1.AppGrantSpec{
			App:     "consumer",
			Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "files", Granted: []string{"read"}}},
		},
	}

	for name, objs := range map[string][]runtime.Object{
		"no grant":     {consumer, provider, tenant.DeepCopy()},
		"narrow grant": {consumer, provider, tenant.DeepCopy(), narrow},
	} {
		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).
			WithStatusSubresource(&gentianov1alpha1.Tenant{}).Build()
		r := &TenantReconciler{Client: c, Scheme: scheme}
		state := &tenantReconcileState{tenant: tenant.DeepCopy()}
		if _, err := r.reconcileTenantStageIntegrations(context.Background(), state); err != nil {
			t.Fatalf("%s: integrations stage: %v", name, err)
		}
		// The request the stage sees: without it this would pass for the
		// wrong reason, on a tenant that asks for nothing.
		asked, err := r.collectDesiredIntegrationBindings(context.Background(), tenant)
		if err != nil || len(asked) != 1 {
			t.Fatalf("%s: bindings asked for = %d (%v), want one", name, len(asked), err)
		}
		var grants gentianov1alpha1.AppGrantList
		if err := c.List(context.Background(), &grants); err != nil {
			t.Fatalf("%s: list grants: %v", name, err)
		}
		switch name {
		case "no grant":
			if len(grants.Items) != 0 {
				t.Fatalf("the reconciler wrote a grant from a declaration: %+v", grants.Items[0].Spec)
			}
		default:
			if len(grants.Items) != 1 {
				t.Fatalf("grants = %d, want the administrator's one", len(grants.Items))
			}
			got := grants.Items[0].Spec.Consume
			if len(got) != 1 || len(got[0].Granted) != 1 || got[0].Granted[0] != "read" {
				t.Fatalf("the administrator's grant was changed to %+v", got)
			}
		}
	}
}

// A binding says how much of what it asks for was granted, so a closed path
// reads as a missing grant and not as a fault.
func TestBindingReportsWhatWasGranted(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	binding := func() *gentianov1alpha1.IntegrationBinding {
		return &gentianov1alpha1.IntegrationBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "demo--consumer--files", Namespace: "tenant-demo"},
			Spec: gentianov1alpha1.IntegrationBindingSpec{
				Contract:     "files",
				Provider:     gentianov1alpha1.AppEndpoint{App: "provider", Namespace: "tenant-demo"},
				Consumer:     gentianov1alpha1.AppEndpoint{App: "consumer", Namespace: "tenant-demo"},
				Capabilities: []string{"read", "write"},
			},
		}
	}
	grant := func(caps ...string) *gentianov1alpha1.AppGrant {
		return &gentianov1alpha1.AppGrant{
			ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "tenant-demo"},
			Spec: gentianov1alpha1.AppGrantSpec{
				App:     "consumer",
				Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "files", Granted: caps}},
			},
		}
	}
	cases := []struct {
		name   string
		grant  *gentianov1alpha1.AppGrant
		status metav1.ConditionStatus
		reason string
	}{
		{"nothing granted", nil, metav1.ConditionFalse, "NotGranted"},
		{"less than asked", grant("read"), metav1.ConditionFalse, "PartlyGranted"},
		{"all of it", grant("read", "write"), metav1.ConditionTrue, "Granted"},
	}
	for _, tc := range cases {
		b := binding()
		builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(b).WithStatusSubresource(b)
		if tc.grant != nil {
			builder = builder.WithObjects(tc.grant)
		}
		c := builder.Build()
		r := &IntegrationBindingReconciler{Client: c, Scheme: scheme}
		key := types.NamespacedName{Name: b.Name, Namespace: b.Namespace}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("%s: Reconcile: %v", tc.name, err)
		}
		got := &gentianov1alpha1.IntegrationBinding{}
		if err := c.Get(context.Background(), key, got); err != nil {
			t.Fatalf("%s: get: %v", tc.name, err)
		}
		var cond *metav1.Condition
		for i := range got.Status.Conditions {
			if got.Status.Conditions[i].Type == conditionBindingGranted {
				cond = &got.Status.Conditions[i]
			}
		}
		if cond == nil || cond.Status != tc.status || cond.Reason != tc.reason {
			t.Fatalf("%s: condition = %+v, want %s/%s", tc.name, cond, tc.status, tc.reason)
		}
	}
}
