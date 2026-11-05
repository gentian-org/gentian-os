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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

func snap(key string, at time.Time) SnapshotObject {
	return SnapshotObject{Key: OpenBaoSnapshotPrefix + key, LastModified: at, Size: 4096}
}

func TestTheNewestSnapshotDecidesTheState(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	fresh := snap("openbao-20261105T021700Z.snap.age", now.Add(-10*time.Hour))
	old := snap("openbao-20261101T021700Z.snap.age", now.Add(-4*day))
	cases := []struct {
		name       string
		storage    string
		storageErr error
		objects    []SnapshotObject
		listErr    error
		want       string
		newest     string
		count      int
	}{
		{name: "a fresh one", storage: "raft", objects: []SnapshotObject{old, fresh}, want: SnapshotCurrent, newest: fresh.Key, count: 2},
		{name: "only an old one", storage: "raft", objects: []SnapshotObject{old}, want: SnapshotStale, newest: old.Key, count: 1},
		{name: "none", storage: "raft", want: SnapshotMissing},
		// What is not a snapshot is not counted: an upload that did not
		// finish, or something somebody put there.
		{name: "only foreign objects", storage: "raft", objects: []SnapshotObject{snap("notes.txt", now), snap("openbao-x.snap.age", now)}, want: SnapshotMissing},
		{name: "the bucket cannot be read", storage: "raft", listErr: errors.New("connection refused"), want: SnapshotUnknown},
		// A vault on file storage takes none, and that is said, whatever
		// old snapshots the bucket still holds.
		{name: "file storage", storage: "file", objects: []SnapshotObject{old}, want: SnapshotNotRaft, newest: old.Key, count: 1},
		// The vault could not be asked; the bucket still answers.
		{name: "the vault cannot be asked", storageErr: errors.New("sealed"), objects: []SnapshotObject{fresh}, want: SnapshotCurrent, newest: fresh.Key, count: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := JudgeOpenBaoSnapshots(now, 30*time.Hour, "b", tc.storage, tc.storageErr, tc.objects, tc.listErr)
			if rep.State != tc.want || rep.Newest != tc.newest || rep.Count != tc.count {
				t.Fatalf("state %s newest %q count %d, want %s %q %d (%s)", rep.State, rep.Newest, rep.Count, tc.want, tc.newest, tc.count, rep.Message)
			}
			if rep.Message == "" || rep.CheckedAt != "2026-11-05T12:00:00Z" || rep.StaleAfter != "30h0m0s" {
				t.Fatalf("report = %+v", rep)
			}
			if tc.newest != "" && (rep.AgeSeconds == nil || *rep.AgeSeconds <= 0) {
				t.Fatalf("ageSeconds = %v", rep.AgeSeconds)
			}
			if tc.storageErr != nil && !strings.Contains(rep.Message, "sealed") {
				t.Fatalf("the message does not say the vault could not be asked: %s", rep.Message)
			}
		})
	}
	if rep := JudgeOpenBaoSnapshots(now, 30*time.Hour, "b", "file", nil, nil, nil); !strings.Contains(rep.Message, "docs/guides/recovery.md §5") {
		t.Fatalf("a vault on file storage is not pointed at the migration: %s", rep.Message)
	}
}

func openBaoSnapshotsFixture(t *testing.T, objs ...client.Object) (client.Client, memVault) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	admin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: storageAdminSecret, Namespace: s3Namespace},
		Data:       map[string][]byte{"endpoint": []byte("http://s3.system-s3.svc.cluster.local:9000")},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, admin)...).Build()
	return c, memVault{}
}

func TestTheSnapshotStoreIsMadeAsAnAppsBucketIs(t *testing.T) {
	ctx := context.Background()
	c, vault := openBaoSnapshotsFixture(t)
	o := &OpenBaoSnapshots{Client: c, Reader: c, Seeder: secrets.NewSeeder(vault, nil), Bucket: "gentian-openbao-snapshots"}

	// First pass: the key is in the vault, the Job is made and handed it.
	if err := o.EnsureStore(ctx); err == nil {
		t.Fatal("the store reads as made before its Job ran")
	}
	rec := vault[OpenBaoSnapshotStorePath]
	if rec["bucket"] != "gentian-openbao-snapshots" || rec["access-key"] == "" || rec["secret-key"] == "" ||
		rec["endpoint"] != "http://s3.system-s3.svc.cluster.local:9000" {
		t.Fatalf("the vault holds %v at %s", rec, OpenBaoSnapshotStorePath)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: openBaoSnapshotBucketJob, Namespace: s3Namespace}, job); err != nil {
		t.Fatal(err)
	}
	if job.Annotations[openBaoSnapshotKeyAnnotation] != snapshotKeyID(rec["access-key"]) {
		t.Fatalf("the Job does not name the key it makes: %v", job.Annotations)
	}
	if job.Spec.Template.Labels[backup.ObjectStorageShellLabel] != "true" {
		t.Fatalf("the Job's pod is not one the object store admits: %v", job.Spec.Template.Labels)
	}
	// The key is in no Job spec: only in the Secret the Job owns.
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Value == rec["secret-key"] || e.Value == rec["access-key"] {
			t.Fatalf("the key is in the Job's spec as %s", e.Name)
		}
	}
	key := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: openBaoSnapshotBucketJob, Namespace: s3Namespace}, key); err != nil {
		t.Fatalf("the Job was not handed its key: %v", err)
	}
	if string(key.Data[backup.ObjectStorageKeySecretKey]) != rec["secret-key"] || len(key.OwnerReferences) != 1 {
		t.Fatalf("the key Secret = %v", key)
	}

	// The Job succeeds: the store is made, and the key leaves the Secret.
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := o.EnsureStore(ctx); err != nil {
		t.Fatalf("the store does not read as made after its Job succeeded: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: openBaoSnapshotBucketJob, Namespace: s3Namespace}, key); !apierrors.IsNotFound(err) {
		t.Fatalf("the key Secret outlived the Job's run: %v", err)
	}
	// And stays made: the next pass makes nothing.
	if err := o.EnsureStore(ctx); err != nil {
		t.Fatalf("a second pass: %v", err)
	}

	// A key the vault holds anew is made again.
	vault[OpenBaoSnapshotStorePath]["access-key"] = "another"
	if err := o.EnsureStore(ctx); err == nil {
		t.Fatal("a new key reads as made")
	}
	if err := c.Get(ctx, types.NamespacedName{Name: openBaoSnapshotBucketJob, Namespace: s3Namespace}, job); !apierrors.IsNotFound(err) {
		t.Fatalf("the Job for the old key was kept: %v", err)
	}
}

func TestTheSnapshotReportIsWrittenOnTheClaimsComposite(t *testing.T) {
	ctx := context.Background()
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gentianos.io/v1alpha1", "kind": "Cluster",
		"metadata": map[string]any{"name": "cluster", "namespace": clusterConfigNamespace},
		"spec": map[string]any{"resourceRef": map[string]any{
			"apiVersion": "gentianos.io/v1alpha1", "kind": "XCluster", "name": "cluster-abc12",
		}},
	}}
	xr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gentianos.io/v1alpha1", "kind": "XCluster",
		"metadata": map[string]any{"name": "cluster-abc12"},
		"status":   map[string]any{"platform": map[string]any{"build": "abc1234"}},
	}}
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).
		WithRuntimeObjects(claim, xr).WithStatusSubresource(xr).Build()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	o := &OpenBaoSnapshots{
		Client: c, Reader: c, Bucket: "gentian-openbao-snapshots",
		Now:       func() time.Time { return now },
		Storage:   func(context.Context) (string, error) { return "file", nil },
		Snapshots: listed(nil),
	}
	rep := o.Once(ctx)
	if rep.State != SnapshotNotRaft {
		t.Fatalf("state = %s (%s)", rep.State, rep.Message)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(xr.GroupVersionKind())
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster-abc12"}, got); err != nil {
		t.Fatal(err)
	}
	status, _, _ := unstructured.NestedMap(got.Object, "status", "openbaoSnapshots")
	if status["state"] != SnapshotNotRaft || status["storage"] != "file" || status["bucket"] != "gentian-openbao-snapshots" ||
		status["checkedAt"] != "2026-11-05T12:00:00Z" {
		t.Fatalf("status.openbaoSnapshots = %v", status)
	}
	// Beside what else is there: a merge.
	if build, _, _ := unstructured.NestedString(got.Object, "status", "platform", "build"); build != "abc1234" {
		t.Fatalf("status.platform was replaced: %v", got.Object["status"])
	}
}

type listed []SnapshotObject

func (l listed) List(context.Context, string, string) ([]SnapshotObject, error) { return l, nil }
