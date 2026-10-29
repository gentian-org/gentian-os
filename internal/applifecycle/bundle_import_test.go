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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// The operator takes a restore up the moment it exists. One created without
// its key was failed for want of it, and the write meant to name the key
// then lost to the operator's status update. The key is named as the restore
// is created, and the Secret holding it goes when the restore does.
func TestARestoreIsCreatedWithItsKey(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	ready := &gentianov1alpha1.TenantExport{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"}}
	ready.Status.Phase = gentianov1alpha1.TenantExportPhaseReady
	ready.Status.Bundle = &gentianov1alpha1.BundleRef{Bucket: "demo-gentian-backup", Prefix: "nightly"}
	s := appsService(t, tenant, ready)
	// The operator, as fast as it is on a cluster: a restore without a key
	// is failed at once.
	s.client = interceptor.NewClient(s.client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			r, ok := obj.(*gentianov1alpha1.TenantRestore)
			if !ok || r.Spec.Decryption != nil {
				return nil
			}
			var failed gentianov1alpha1.TenantRestore
			if err := c.Get(ctx, client.ObjectKeyFromObject(r), &failed); err != nil {
				return err
			}
			failed.Status.Phase = "Failed"
			return c.Update(ctx, &failed)
		},
	})
	key := Decryption{Identity: "AGE-SECRET-KEY-1GFPYYSJZGFPYYSJZGFPYYSJZGFPYYSJZGFPYYSJZGFPYYSJZGFPQ4EGAEX"}

	got, err := s.StartRestore(context.Background(), "demo", RestoreRequest{Backup: "nightly", Decryption: key}, "tom")
	if err != nil {
		t.Fatal(err)
	}
	var restore gentianov1alpha1.TenantRestore
	if err := s.client.Get(context.Background(), types.NamespacedName{Name: got.Name, Namespace: "tenant-demo"}, &restore); err != nil {
		t.Fatal(err)
	}
	if restore.Status.Phase == "Failed" || restore.Spec.Decryption == nil || restore.Spec.Decryption.IdentitySecretRef == nil {
		t.Fatalf("restore created without its key: spec %+v, phase %q", restore.Spec, restore.Status.Phase)
	}
	var secret corev1.Secret
	if err := s.client.Get(context.Background(), types.NamespacedName{Name: restore.Spec.Decryption.IdentitySecretRef.Name, Namespace: "tenant-demo"}, &secret); err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(&secret); owner == nil || owner.Name != restore.Name {
		t.Errorf("the key's owner is %+v, not restore %s", owner, restore.Name)
	}

	// A second restore under a name taken leaves the first one's key as it was.
	if _, err := s.StartRestore(context.Background(), "demo", RestoreRequest{Name: got.Name, Backup: "nightly", Decryption: key}, "tom"); err == nil {
		t.Fatal("a second restore of the same name was created")
	}
	if err := s.client.Get(context.Background(), types.NamespacedName{Name: got.Name + "-key", Namespace: "tenant-demo"}, &secret); err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(&secret); owner == nil || owner.Name != restore.Name {
		t.Errorf("the first restore's key was replaced or lost its owner: %+v", owner)
	}
}
