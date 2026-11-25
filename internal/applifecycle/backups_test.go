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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A screen that offers a person their own exports must tell them apart from
// the ones a schedule took.
func TestABackupSaysWhichScheduleTookIt(t *testing.T) {
	scheduled := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{
		Name:   "nightly-20261105-0300",
		Labels: map[string]string{gentianov1alpha1.TenantExportScheduleLabel: "nightly"},
	}}
	if got := backupSummary(scheduled, "demo").Schedule; got != "nightly" {
		t.Errorf("scheduled export: Schedule = %q, want %q", got, "nightly")
	}

	manual := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "export-2026-11-05-10-00"}}
	if got := backupSummary(manual, "demo").Schedule; got != "" {
		t.Errorf("manual export: Schedule = %q, want empty", got)
	}
}

// What is exported before a move is read as it applies: the tenant's own
// where it states one and may, else the cluster's, else app; and whether the
// tenant may state one is the cluster's to say.
func TestTheBackupPolicySaysWhatIsExportedBeforeAMove(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	no := false
	cluster := func(beforeMove gentianov1alpha1.BeforeMoveExport, allow *bool) *gentianov1alpha1.BackupPolicy {
		return &gentianov1alpha1.BackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec:   gentianov1alpha1.BackupPolicySpec{Scope: "cluster", BeforeMove: beforeMove, AllowTenantOverride: allow},
			Status: gentianov1alpha1.BackupPolicyStatus{EffectiveBeforeMove: beforeMove}}
	}
	own := &gentianov1alpha1.BackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:   gentianov1alpha1.BackupPolicySpec{Scope: "tenant", Tenant: "demo", BeforeMove: gentianov1alpha1.BeforeMoveNone},
		Status: gentianov1alpha1.BackupPolicyStatus{EffectiveBeforeMove: gentianov1alpha1.BeforeMoveNone}}
	for _, tc := range []struct {
		name            string
		objects         []client.Object
		stated, applies string
		allowed         bool
	}{
		{"no policy", nil, "", "app", true},
		{"the cluster's", []client.Object{cluster("tenant", nil)}, "", "tenant", true},
		{"the tenant's own", []client.Object{cluster("tenant", nil), own}, "none", "none", true},
		{"the tenant's own, refused", []client.Object{cluster("tenant", &no), own}, "none", "tenant", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := append([]client.Object{&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}}, tc.objects...)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			got, err := (&Service{client: c}).BackupPolicy(context.Background(), "tenant", "demo")
			if err != nil {
				t.Fatal(err)
			}
			if got.BeforeMove != tc.stated || got.EffectiveBeforeMove != tc.applies || got.AllowTenantOverride != tc.allowed {
				t.Fatalf("stated %q, applies %q, allowed %v; want %q, %q, %v",
					got.BeforeMove, got.EffectiveBeforeMove, got.AllowTenantOverride, tc.stated, tc.applies, tc.allowed)
			}
		})
	}
}
