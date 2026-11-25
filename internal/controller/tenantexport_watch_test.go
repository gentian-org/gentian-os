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

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// A capture Job wakes the export it works for, wherever the Job runs: the
// completion is recorded on the pass it causes, not on a requeue the Job may
// not outlive. A restore's Job, or one nobody's run made, wakes no export.
func TestACaptureJobWakesItsExport(t *testing.T) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, gentianov1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	r := &TenantExportReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(tenant).Build(), Scheme: s}
	ns := backup.TenantNamespace(tenant)

	job := func(labels map[string]string) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "tx-acme-move-wiki-pg", Namespace: "system-postgresql", Labels: labels}}
	}
	ours := map[string]string{
		meta.TenantLabel: "acme", meta.ManagedByLabel: meta.ManagedByValue,
		backup.ExportLabel: "move-wiki-1", backup.RunKindLabel: backup.RunKindExport,
	}
	got := r.exportOfJob(context.Background(), job(ours))
	if len(got) != 1 || got[0].NamespacedName != (types.NamespacedName{Name: "move-wiki-1", Namespace: ns}) {
		t.Fatalf("a capture Job woke %v, want move-wiki-1 in %s", got, ns)
	}

	without := func(key string, value ...string) map[string]string {
		l := map[string]string{}
		for k, v := range ours {
			l[k] = v
		}
		delete(l, key)
		if len(value) == 1 {
			l[key] = value[0]
		}
		return l
	}
	for what, labels := range map[string]map[string]string{
		"a restore's Job":                 without(backup.RunKindLabel, backup.RunKindRestore),
		"a Job naming no export":          without(backup.ExportLabel),
		"a Job naming no tenant":          without(meta.TenantLabel),
		"a Job the platform did not make": without(meta.ManagedByLabel, "someone"),
	} {
		if got := r.exportOfJob(context.Background(), job(labels)); len(got) != 0 {
			t.Errorf("%s woke %v", what, got)
		}
	}
}
