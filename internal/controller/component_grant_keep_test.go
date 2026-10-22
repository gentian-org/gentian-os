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
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// A privilege approved again on the Tenant -- a second press of the button
// stamps a new approvedAt -- does not rewrite the approval the Component
// carries: the Component's API refuses a changed approver or approvedAt, and
// that refusal failed the tenant's whole app stage. The stored approval
// stays as it is, a new privilege is added, and a withdrawn one is removed.
func TestAStoredApprovalIsNeverRewritten(t *testing.T) {
	ctx := context.Background()
	scheme := componentDatabaseScheme(t)
	tenant := acmeTenantFixture()
	first := metav1.NewTime(time.Date(2026, 10, 22, 7, 53, 36, 0, time.UTC))
	stored := []gentianov1alpha1.PrivilegeGrant{
		{Privilege: "podSecurity/restrict-capabilities", Approver: "u-owner",
			ApprovedAt: first, Reason: "the office suite only runs this way"},
		{Privilege: "egress/webhooks", Approver: "u-owner",
			ApprovedAt: first, Reason: "approved, and later withdrawn"},
	}
	existing := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "office", Namespace: tenantNamespaceName(tenant)},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: "office"},
			Class:      gentianov1alpha1.ComponentClassApp,
			Privileges: stored,
		},
	}
	again := metav1.NewTime(first.Add(31 * time.Second))
	tenant.Spec.Privileges = []gentianov1alpha1.TenantPrivilegeGrant{
		{Install: "office", Privilege: "podSecurity/restrict-capabilities", Approver: "u-other",
			ApprovedAt: again, Reason: "approved a second time"},
		{Install: "office", Privilege: "podSecurity/require-seccomp", Approver: "u-owner",
			ApprovedAt: again, Reason: "a privilege approved for the first time"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(), existing,
		profileFixture("office", false, gentianov1alpha1.ComponentClassApp),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	if err := r.ensureComponent(ctx, tenant, "office", "", "", nil, nil, nil, moveRecord{}); err != nil {
		t.Fatal(err)
	}
	got := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, types.NamespacedName{Name: "office", Namespace: tenantNamespaceName(tenant)}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Privileges) != 2 {
		t.Fatalf("grants = %+v, want the kept one and the new one", got.Spec.Privileges)
	}
	kept := got.Spec.Privileges[0]
	if kept.Privilege != stored[0].Privilege || kept.Approver != stored[0].Approver ||
		!kept.ApprovedAt.Equal(&stored[0].ApprovedAt) || kept.Reason != stored[0].Reason {
		t.Errorf("the stored approval was rewritten: %+v, want %+v", kept, stored[0])
	}
	added := got.Spec.Privileges[1]
	if added.Privilege != "podSecurity/require-seccomp" || added.Approver != "u-owner" || !added.ApprovedAt.Equal(&again) {
		t.Errorf("the new approval = %+v", added)
	}
}

// One app whose Component cannot be written stops the app stage, and the
// mail records every app that declares mail waits for are written all the
// same, because they are written before the apps.
func TestAppMailRecordsAreSeededAlthoughAnAppFails(t *testing.T) {
	ctx := context.Background()
	scheme := componentDatabaseScheme(t)
	tenant := mailingTenant()
	tenant.Spec.Apps = append(tenant.Spec.Apps, gentianov1alpha1.TenantApp{Profile: "broken"})
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(mailingProfile(), profileFixture("broken", false, gentianov1alpha1.ComponentClassApp), tenant.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*gentianov1alpha1.Component); ok && obj.GetName() == "broken" {
					return errors.New("refused by the API server")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	vault := memVault{}
	r := &TenantReconciler{Client: c, Scheme: scheme, Seeder: secrets.NewSeeder(vault, nil)}
	state := &tenantReconcileState{tenant: tenant, nsName: tenantNamespaceName(tenant)}

	if _, err := r.reconcileTenantStageAppsAndEdge(ctx, state); err == nil {
		t.Fatal("the app stage succeeded although an app's Component was refused")
	}
	for _, rec := range []string{"smtp", "imap"} {
		if _, ok := vault[secrets.CategoryPath("quiet", "mailer", rec)]; !ok {
			t.Errorf("no %s record for the mailing app after another app failed", rec)
		}
	}
}
