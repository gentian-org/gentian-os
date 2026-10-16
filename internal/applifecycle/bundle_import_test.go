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
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What makes a restore other than the guaranteed round trip is said in a
// field of its own, and does not take the place of where the restore is.
func TestARestoreSaysWhereItIsOutsideTheGuaranteeApartFromWhereItIs(t *testing.T) {
	r := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "import-1"}}
	r.Status.Conditions = []metav1.Condition{
		{Type: "OutsideGuarantee", Status: metav1.ConditionTrue, Reason: "OtherVersion", Message: "written by platform version 0.4"},
		{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted", Message: "restoring 2 app(s)"},
	}
	got := restoreStatus(r, "demo")
	if got.OutsideGuarantee != "written by platform version 0.4" || got.Message != "restoring 2 app(s)" {
		t.Errorf("outsideGuarantee %q, message %q", got.OutsideGuarantee, got.Message)
	}

	r.Status.Conditions = r.Status.Conditions[1:]
	if got := restoreStatus(r, "demo"); got.OutsideGuarantee != "" {
		t.Errorf("a restore within the guarantee says %q", got.OutsideGuarantee)
	}
}

// A restore of one of the tenant's own backups names it, and the bundle is
// the one its TenantExport records in the tenant's own namespace. A backup
// that has not finished, or is another tenant's, is not restored from.
func TestARestoreOfABackupIsOfTheTenantsOwnExport(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	ready := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"}}
	ready.Status.Phase = gentianov1alpha1.TenantExportPhaseReady
	ready.Status.Bundle = &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"}
	running := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "now", Namespace: "tenant-demo"}}
	running.Status.Phase = gentianov1alpha1.TenantExportPhaseRunning
	other := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "theirs", Namespace: "tenant-solo"}}
	other.Status.Phase = gentianov1alpha1.TenantExportPhaseReady
	other.Status.Bundle = &gentianov1alpha1.BundleRef{Bucket: "solo-gentian-backup", Prefix: "theirs"}
	s := appsService(t, tenant, ready, running, other)
	key := Decryption{Identity: "AGE-SECRET-KEY-1GFPYYSJZGFPYYSJZGFPYYSJZGFPYYSJZGFPYYSJZGFPYYSJZGFPQ4EGAEX"}

	got, err := s.StartRestore(context.Background(), "demo", RestoreRequest{Backup: "nightly", Decryption: key}, "tom")
	if err != nil {
		t.Fatal(err)
	}
	var restore gentianov1alpha1.TenantRestore
	if err := s.client.Get(context.Background(), types.NamespacedName{Name: got.Name, Namespace: "tenant-demo"}, &restore); err != nil {
		t.Fatal(err)
	}
	if restore.Spec.ExportRef != "nightly" || restore.Spec.Bundle != nil || restore.Spec.ConfirmTenant != "demo" ||
		restore.Spec.Decryption == nil || restore.Spec.Decryption.IdentitySecretRef == nil {
		t.Fatalf("restore spec = %+v", restore.Spec)
	}

	for name, req := range map[string]RestoreRequest{
		"unfinished":       {Backup: "now", Decryption: key},
		"another tenant's": {Backup: "theirs", Decryption: key},
		"both":             {Backup: "nightly", Bundle: gentianov1alpha1.BundleRef{Bucket: "gentian-imports", Prefix: "u"}, Decryption: key},
	} {
		if _, err := s.StartRestore(context.Background(), "demo", req, "tom"); err == nil {
			t.Errorf("%s: a restore was started", name)
		}
	}
}
