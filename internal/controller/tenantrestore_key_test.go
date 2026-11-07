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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const restoreKeyTestUID = "3f0c9a52-7d1e-4c55-9a7e-1b2c3d4e5f60"

// newRestoreKeyWorld is newRestoreWorld with a restore that has a UID, and
// its key Secret controlled by it when owned is true.
func newRestoreKeyWorld(t *testing.T, owned bool) *restoreWorld {
	t.Helper()
	w := newRestoreWorld(t, func(r *gentianov1alpha1.TenantRestore) { r.UID = restoreKeyTestUID })
	if !owned {
		return w
	}
	ctx := context.Background()
	key := &corev1.Secret{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "r1-key", Namespace: "tenant-demo"}, key); err != nil {
		t.Fatal(err)
	}
	if err := controllerutil.SetControllerReference(w.restore(t), key, w.r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Update(ctx, key); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *restoreWorld) keyExists(t *testing.T) bool {
	t.Helper()
	err := w.c.Get(context.Background(), types.NamespacedName{Name: "r1-key", Namespace: "tenant-demo"}, &corev1.Secret{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// The key stored for a restore, which the restore controls, is deleted when
// the restore ends, Ready or Failed: the restore is kept as a record, the key
// is not kept with it.
func TestTheKeyARestoreControlsIsDeletedWhenItEnds(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		w := newRestoreKeyWorld(t, true)
		if got := w.run(t); got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
			t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
		}
		if w.keyExists(t) {
			t.Error("the restore ended Ready and its key is still in the tenant's namespace")
		}
	})
	t.Run("failed before it began", func(t *testing.T) {
		w := newRestoreKeyWorld(t, true)
		w.bundles.err = errors.New("the manifest could not be opened with that key")
		if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatal(err)
		}
		if got := w.restore(t); got.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed {
			t.Fatalf("phase = %s", got.Status.Phase)
		}
		if w.keyExists(t) {
			t.Error("the restore failed and its key is still in the tenant's namespace")
		}
	})
	t.Run("failed after it began", func(t *testing.T) {
		w := newRestoreKeyWorld(t, true)
		ctx := context.Background()
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatal(err)
		}
		began := w.restore(t)
		if began.Status.StartedAt == nil {
			t.Fatal("the restore did not begin")
		}
		if _, err := w.r.fail(ctx, began, "RestoreFailed", "wiki: restore did not succeed"); err != nil {
			t.Fatal(err)
		}
		if w.keyExists(t) {
			t.Error("the restore failed and its key is still in the tenant's namespace")
		}
	})
}

// A key Secret the restore does not control is a person's, and stays when
// the restore ends, whichever way it ends.
func TestAKeyARestoreDoesNotControlIsLeftWhenItEnds(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		w := newRestoreKeyWorld(t, false)
		if got := w.run(t); got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
			t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
		}
		if !w.keyExists(t) {
			t.Error("a key the restore does not control was deleted")
		}
	})
	t.Run("failed", func(t *testing.T) {
		w := newRestoreKeyWorld(t, false)
		w.bundles.err = errors.New("the manifest could not be opened with that key")
		if _, err := w.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatal(err)
		}
		if !w.keyExists(t) {
			t.Error("a key the restore does not control was deleted")
		}
	})
	t.Run("controlled by another restore", func(t *testing.T) {
		w := newRestoreKeyWorld(t, false)
		ctx := context.Background()
		key := &corev1.Secret{}
		if err := w.c.Get(ctx, types.NamespacedName{Name: "r1-key", Namespace: "tenant-demo"}, key); err != nil {
			t.Fatal(err)
		}
		controller := true
		key.OwnerReferences = []metav1.OwnerReference{{APIVersion: gentianov1alpha1.GroupVersion.String(),
			Kind: "TenantRestore", Name: "r1", UID: "an-earlier-r1", Controller: &controller}}
		if err := w.c.Update(ctx, key); err != nil {
			t.Fatal(err)
		}
		w.bundles.err = errors.New("the manifest could not be opened with that key")
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatal(err)
		}
		if !w.keyExists(t) {
			t.Error("a key controlled by another restore of the same name was deleted")
		}
	})
}

// The key is stored before the restore is created and made the restore's
// after it, so a restore can end while its key is not yet its own. It passes
// the key over then; the reconcile that follows the key becoming its own
// deletes it.
func TestAKeyMadeTheRestoresAfterItEndedIsDeletedThen(t *testing.T) {
	w := newRestoreKeyWorld(t, false)
	ctx := context.Background()
	w.bundles.err = errors.New("the manifest could not be opened with that key")
	if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	if got := w.restore(t); got.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed {
		t.Fatalf("phase = %s", got.Status.Phase)
	}
	if !w.keyExists(t) {
		t.Fatal("a key that was not yet the restore's was deleted")
	}

	key := &corev1.Secret{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "r1-key", Namespace: "tenant-demo"}, key); err != nil {
		t.Fatal(err)
	}
	if err := controllerutil.SetControllerReference(w.restore(t), key, w.r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Update(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
		t.Fatal(err)
	}
	if w.keyExists(t) {
		t.Error("the ended restore became the key's controller and the key is still there")
	}
}
