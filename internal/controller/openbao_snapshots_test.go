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
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

var (
	vaultInstance   = SnapshotInstance{Name: "openbao", What: "the vault", Job: "openbao-snapshot"}
	transitInstance = SnapshotInstance{Name: "openbao-transit", What: "the transit seal", Job: "openbao-transit-snapshot"}
)

func snap(key string, at time.Time) SnapshotObject {
	return SnapshotObject{Key: "openbao/" + key, LastModified: at, Size: 4096}
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
			rep := JudgeOpenBaoSnapshots(now, 30*time.Hour, "b", vaultInstance, tc.storage, tc.storageErr, tc.objects, tc.listErr)
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
	if rep := JudgeOpenBaoSnapshots(now, 30*time.Hour, "b", vaultInstance, "file", nil, nil, nil); !strings.Contains(rep.Message, "docs/guides/recovery.md §5") {
		t.Fatalf("a vault on file storage is not pointed at the migration: %s", rep.Message)
	}
}

// The transit seal is not asked: the marker its job leaves says what
// storage it is on, and only its own prefix's objects count.
func TestTheSealsSnapshotsAreJudgedUnderItsOwnPrefix(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	tsnap := func(key string, at time.Time) SnapshotObject {
		return SnapshotObject{Key: "openbao-transit/" + key, LastModified: at}
	}
	fresh := tsnap("openbao-transit-20261105T021700Z.snap.age", now.Add(-time.Hour))
	cases := []struct {
		name    string
		objects []SnapshotObject
		want    string
		storage string
	}{
		{name: "a fresh one, marked raft", objects: []SnapshotObject{fresh, tsnap("storage.raft", now.Add(-time.Hour))}, want: SnapshotCurrent, storage: "raft"},
		{name: "marked file", objects: []SnapshotObject{tsnap("storage.file", now)}, want: SnapshotNotRaft, storage: "file"},
		{name: "the newer marker counts", objects: []SnapshotObject{fresh, tsnap("storage.file", now.Add(-48*time.Hour)), tsnap("storage.raft", now.Add(-time.Hour))}, want: SnapshotCurrent, storage: "raft"},
		{name: "nothing yet", want: SnapshotMissing},
		// The vault's snapshots are not the seal's.
		{name: "a vault snapshot is not one", objects: []SnapshotObject{tsnap("openbao-20261105T021700Z.snap.age", now)}, want: SnapshotMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := JudgeOpenBaoSnapshots(now, 30*time.Hour, "b", transitInstance, "", nil, tc.objects, nil)
			if rep.State != tc.want || rep.Storage != tc.storage || rep.Instance != "openbao-transit" {
				t.Fatalf("state %s storage %q instance %q, want %s %q (%s)", rep.State, rep.Storage, rep.Instance, tc.want, tc.storage, rep.Message)
			}
			if tc.want == SnapshotMissing && !strings.Contains(rep.Message, "openbao-transit-snapshot") {
				t.Fatalf("the message does not name the seal's CronJob: %s", rep.Message)
			}
		})
	}
}

// metaVault is a vault that keeps KV v2 metadata: each record's
// created_time, new when it is written after a delete, and its version, one
// more on every write.
type metaVault struct {
	data  memVault
	meta  map[string]secrets.RecordMetadata
	clock int
}

func newMetaVault() *metaVault {
	return &metaVault{data: memVault{}, meta: map[string]secrets.RecordMetadata{}}
}

func (v *metaVault) wrote(path string) {
	m, ok := v.meta[path]
	if !ok {
		v.clock++
		m = secrets.RecordMetadata{CreatedTime: fmt.Sprintf("2026-11-05T12:00:%02dZ", v.clock)}
	}
	m.CurrentVersion++
	v.meta[path] = m
}

func (v *metaVault) PutOnce(ctx context.Context, path string, data map[string]string) error {
	if _, ok := v.data[path]; ok {
		return nil
	}
	v.wrote(path)
	return v.data.PutOnce(ctx, path, data)
}

func (v *metaVault) Put(ctx context.Context, path string, data map[string]string) error {
	v.wrote(path)
	return v.data.Put(ctx, path, data)
}

func (v *metaVault) Get(ctx context.Context, path string) (map[string]string, error) {
	return v.data.Get(ctx, path)
}

func (v *metaVault) Metadata(_ context.Context, path string) (secrets.RecordMetadata, error) {
	m, ok := v.meta[path]
	if !ok {
		return secrets.RecordMetadata{}, secrets.ErrNotFound
	}
	return m, nil
}

// remove deletes a record with all its versions, as kv metadata delete does.
func (v *metaVault) remove(path string) {
	delete(v.data, path)
	delete(v.meta, path)
}

func openBaoSnapshotsFixture(t *testing.T, objs ...client.Object) (client.Client, *metaVault) {
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
	return c, newMetaVault()
}

const testS3Endpoint = "http://s3.system-s3.svc.cluster.local:9000"

func TestTheSnapshotStoreIsMadeAsAnAppsBucketIs(t *testing.T) {
	ctx := context.Background()
	c, vault := openBaoSnapshotsFixture(t)
	o := &OpenBaoSnapshots{Client: c, Reader: c, Seeder: secrets.NewSeeder(vault, nil), Bucket: "gentian-openbao-snapshots"}
	inst := o.instances()[0]
	path := inst.StorePath()
	if path != "gentian-os/kernel/openbao-snapshots/openbao" {
		t.Fatalf("the vault's store record is at %s", path)
	}

	// First pass: the key is in the vault, the Job is made and handed it on
	// the same pass. Being made is no error.
	ready, err := o.EnsureStore(ctx, inst, testS3Endpoint)
	if ready || err != nil {
		t.Fatalf("first pass: ready %v, error %v; want not ready and no error", ready, err)
	}
	rec := vault.data[path]
	if rec["bucket"] != "gentian-openbao-snapshots" || rec["access-key"] == "" || rec["secret-key"] == "" ||
		rec["endpoint"] != testS3Endpoint {
		t.Fatalf("the vault holds %v at %s", rec, path)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: "s3-bucket-kernel-openbao-snapshots", Namespace: s3Namespace}, job); err != nil {
		t.Fatal(err)
	}
	if want := snapshotStoreFingerprint(vault.meta[path], "gentian-openbao-snapshots", "openbao/"); job.Annotations[openBaoSnapshotKeyAnnotation] != want {
		t.Fatalf("the Job is annotated %v, want %s", job.Annotations, want)
	}
	// The annotation names the record, never the key.
	if a := job.Annotations[openBaoSnapshotKeyAnnotation]; strings.Contains(a, rec["access-key"]) || strings.Contains(a, rec["secret-key"]) {
		t.Fatalf("the annotation carries the key: %s", a)
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
	if err := c.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: s3Namespace}, key); err != nil {
		t.Fatalf("the Job was not handed its key on the pass that made it: %v", err)
	}
	if string(key.Data[backup.ObjectStorageKeySecretKey]) != rec["secret-key"] || len(key.OwnerReferences) != 1 {
		t.Fatalf("the key Secret = %v", key)
	}

	// The Job succeeds: the store is made, and the key leaves the Secret.
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if ready, err := o.EnsureStore(ctx, inst, testS3Endpoint); !ready || err != nil {
		t.Fatalf("the store does not read as made after its Job succeeded: %v %v", ready, err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: s3Namespace}, key); !apierrors.IsNotFound(err) {
		t.Fatalf("the key Secret outlived the Job's run: %v", err)
	}
	// And stays made: the next pass makes nothing.
	if ready, err := o.EnsureStore(ctx, inst, testS3Endpoint); !ready || err != nil {
		t.Fatalf("a second pass: %v %v", ready, err)
	}

	// The record deleted and seeded anew -- version 1 again, but a new
	// created_time -- is a new key, and the Job runs again.
	oldKey := vault.data[path]["access-key"]
	vault.remove(path)
	if ready, err := o.EnsureStore(ctx, inst, testS3Endpoint); ready || err != nil {
		t.Fatalf("a re-seeded record: ready %v, error %v", ready, err)
	}
	if vault.meta[path].CurrentVersion != 1 || vault.data[path]["access-key"] == oldKey {
		t.Fatalf("the record was not seeded anew: %v %v", vault.meta[path], vault.data[path])
	}
	if err := c.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: s3Namespace}, job); !apierrors.IsNotFound(err) {
		t.Fatalf("the Job for the old key was kept: %v", err)
	}
	if _, err := o.EnsureStore(ctx, inst, testS3Endpoint); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: s3Namespace}, job); err != nil {
		t.Fatalf("no Job for the new key: %v", err)
	}
	if want := snapshotStoreFingerprint(vault.meta[path], "gentian-openbao-snapshots", "openbao/"); job.Annotations[openBaoSnapshotKeyAnnotation] != want {
		t.Fatalf("the new Job is annotated %v, want %s", job.Annotations, want)
	}
}

// Without the record's metadata there is no fingerprint, and no Job: the
// key is never hashed in its place.
func TestNoRecordMetadataIsAnError(t *testing.T) {
	ctx := context.Background()
	c, _ := openBaoSnapshotsFixture(t)
	o := &OpenBaoSnapshots{Client: c, Reader: c, Seeder: secrets.NewSeeder(memVault{}, nil), Bucket: "gentian-openbao-snapshots"}
	inst := o.instances()[0]
	if ready, err := o.EnsureStore(ctx, inst, testS3Endpoint); ready || err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("ready %v, error %v; want the metadata error", ready, err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: inst.bucketJob(), Namespace: s3Namespace}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a Job was made without a fingerprint: %v", err)
	}
}

// Each instance has its own key, scoped to its own prefix of the bucket: the
// vault's job cannot touch the seal's snapshots, nor the seal's the vault's.
func TestEachInstanceHasAKeyForItsOwnPrefix(t *testing.T) {
	ctx := context.Background()
	c, vault := openBaoSnapshotsFixture(t)
	o := &OpenBaoSnapshots{Client: c, Reader: c, Seeder: secrets.NewSeeder(vault, nil), Bucket: "gentian-openbao-snapshots"}
	scripts := map[string]string{}
	for _, inst := range o.instances() {
		if _, err := o.EnsureStore(ctx, inst, testS3Endpoint); err != nil {
			t.Fatal(err)
		}
		job := &batchv1.Job{}
		if err := c.Get(ctx, types.NamespacedName{Name: inst.bucketJob(), Namespace: s3Namespace}, job); err != nil {
			t.Fatal(err)
		}
		scripts[inst.Name] = strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
	}
	vaultRec, sealRec := vault.data["gentian-os/kernel/openbao-snapshots/openbao"], vault.data["gentian-os/kernel/openbao-snapshots/openbao-transit"]
	if vaultRec == nil || sealRec == nil || vaultRec["access-key"] == sealRec["access-key"] || vaultRec["secret-key"] == sealRec["secret-key"] {
		t.Fatalf("the two instances do not have keys of their own: %v %v", vaultRec, sealRec)
	}
	for name, want := range map[string]string{
		"openbao":         "-user=kernel_openbao-snapshot -access_key=${APP_ACCESS_KEY} -secret_key=${APP_SECRET_KEY} -buckets=gentian-openbao-snapshots/openbao/* -actions=Read,Write,List -apply",
		"openbao-transit": "-user=kernel_openbao-transit-snapshot -access_key=${APP_ACCESS_KEY} -secret_key=${APP_SECRET_KEY} -buckets=gentian-openbao-snapshots/openbao-transit/* -actions=Read,Write,List -apply",
	} {
		if !strings.Contains(scripts[name], want) {
			t.Fatalf("the bucket Job of %s does not scope its identity to its prefix:\n%s", name, scripts[name])
		}
	}
}

// The bucket is the operator's to name: a bucket changed in the chart reaches
// the record the jobs read, and the identity is made again for it. The key
// pair stays.
func TestTheStoreFollowsTheBucket(t *testing.T) {
	ctx := context.Background()
	c, vault := openBaoSnapshotsFixture(t)
	o := &OpenBaoSnapshots{Client: c, Reader: c, Seeder: secrets.NewSeeder(vault, nil), Bucket: "gentian-openbao-snapshots"}
	inst := o.instances()[0]
	if _, err := o.EnsureStore(ctx, inst, testS3Endpoint); err != nil {
		t.Fatal(err)
	}
	before := vault.data[inst.StorePath()]["access-key"]
	job := &batchv1.Job{}
	name := types.NamespacedName{Name: inst.bucketJob(), Namespace: s3Namespace}
	if err := c.Get(ctx, name, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	o.Bucket = "elsewhere"
	if ready, err := o.EnsureStore(ctx, inst, "http://s3.moved:9000"); ready || err != nil {
		t.Fatalf("a new bucket: ready %v, error %v", ready, err)
	}
	rec := vault.data[inst.StorePath()]
	if rec["bucket"] != "elsewhere" || rec["endpoint"] != "http://s3.moved:9000" || rec["access-key"] != before {
		t.Fatalf("the record did not follow the bucket, or lost its key: %v", rec)
	}
	if err := c.Get(ctx, name, job); !apierrors.IsNotFound(err) {
		t.Fatalf("the Job for the old bucket was kept: %v", err)
	}
	if _, err := o.EnsureStore(ctx, inst, "http://s3.moved:9000"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, name, job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(job.Spec.Template.Spec.Containers[0].Command, " "), "-buckets=elsewhere/openbao/*") {
		t.Fatalf("the new Job does not make the identity for the new bucket")
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
	patches := 0
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).
		WithRuntimeObjects(claim, xr).WithStatusSubresource(xr).
		WithInterceptorFuncs(interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patches++
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		}}).Build()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	o := &OpenBaoSnapshots{
		Client: c, Reader: c, Bucket: "gentian-openbao-snapshots",
		Now:       func() time.Time { return now },
		Storage:   func(context.Context) (string, error) { return "file", nil },
		Snapshots: listed(nil),
	}
	reps := o.Once(ctx)
	if len(reps) != 2 || reps[0].State != SnapshotNotRaft || reps[1].Instance != "openbao-transit" || reps[1].State != SnapshotMissing {
		t.Fatalf("reports = %+v", reps)
	}
	if patches != 1 {
		t.Fatalf("the two reports took %d status patches, want one", patches)
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
	// The seal's report, beside the vault's.
	if seal, _, _ := unstructured.NestedMap(got.Object, "status", "transitSnapshots"); seal["state"] != SnapshotMissing || seal["instance"] != "openbao-transit" {
		t.Fatalf("status.transitSnapshots = %v", seal)
	}
	// Beside what else is there: a merge.
	if build, _, _ := unstructured.NestedString(got.Object, "status", "platform", "build"); build != "abc1234" {
		t.Fatalf("status.platform was replaced: %v", got.Object["status"])
	}
}

// A cluster whose values switch the seal's snapshots off: the seal is
// reported Disabled, not Missing, and no store is made for it.
func TestADisabledSealIsReportedSoAndGetsNoStore(t *testing.T) {
	ctx := context.Background()
	c, vault := openBaoSnapshotsFixture(t)
	o := &OpenBaoSnapshots{
		Client: c, Reader: c, Seeder: secrets.NewSeeder(vault, nil), Bucket: "gentian-openbao-snapshots",
		TransitDisabled: true,
		Storage:         func(context.Context) (string, error) { return "raft", nil },
		Snapshots:       listed(nil),
	}
	reps := o.Once(ctx)
	if len(reps) != 2 || reps[1].Instance != "openbao-transit" || reps[1].State != SnapshotDisabled ||
		!strings.Contains(reps[1].Message, "openbaoSnapshots.transit.enabled") {
		t.Fatalf("reports = %+v", reps)
	}
	if _, ok := vault.data["gentian-os/kernel/openbao-snapshots/openbao-transit"]; ok {
		t.Fatal("a key was made for the seal's switched-off snapshots")
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "s3-bucket-kernel-openbao-transit-snapshots", Namespace: s3Namespace}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a bucket Job was made for the seal: %v", err)
	}
	// The vault's store is being made: said neutrally, and the next pass
	// comes soon.
	if reps[0].State != SnapshotMissing || !strings.HasSuffix(reps[0].Message, "The operator is making the snapshot store.") || !o.pending {
		t.Fatalf("the vault's report while its store is made: %+v (pending %v)", reps[0], o.pending)
	}
}

type listed []SnapshotObject

func (l listed) List(context.Context, string, string) ([]SnapshotObject, error) { return l, nil }
