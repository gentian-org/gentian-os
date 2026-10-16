/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bundlestore

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore/s3test"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// importsWorld is the cluster's own storage, in memory, and a client that
// holds its keys and the restores given.
func importsWorld(t *testing.T, objects ...client.Object) (*s3test.Server, *Store, client.Client) {
	t.Helper()
	s3 := s3test.New(t)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects = append(objects, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backup.StorageAdminSecret, Namespace: layout.System("s3")},
		Data: map[string][]byte{"endpoint": []byte(s3.URL),
			backup.DestinationAccessKeyField: []byte("a"), backup.DestinationSecretKeyField: []byte("b")},
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return s3, &Store{Client: c}, c
}

func record(t *testing.T, rec Record) []byte {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// What has been in the import bucket longer than a week goes, by its record
// or by the time its name carries, unless a restore that has not ended reads
// it; a record left without a bundle goes as well, and nothing newer is
// touched.
func TestTheSweepRemovesWhatIsOlderThanAWeekAndNothingInUse(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-8*24*time.Hour), now.Add(-time.Hour)
	running := &gentianov1alpha1.TenantRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "tenant-demo"},
		Spec:       gentianov1alpha1.TenantRestoreSpec{Bundle: &gentianov1alpha1.BundleRef{Bucket: ImportBucket, Prefix: "20261010-100000-cccccccc"}},
	}
	s3, store, c := importsWorld(t, running)
	put := func(prefix string, rec *Record) {
		s3.Put(ImportBucket, prefix+"/manifest.json.age", []byte("x"), old)
		if rec != nil {
			s3.Put(ImportBucket, RecordKey(prefix), record(t, *rec), old)
		}
	}
	put("20261010-100000-aaaaaaaa", &Record{Kind: "upload", At: old})                                           // old, recorded
	put("20261020-110000-bbbbbbbb", &Record{Kind: "fetch", At: recent})                                         // recent
	put("20261010-100000-cccccccc", &Record{Kind: "upload", At: old})                                           // old, in use
	put("20261010-100000-dddddddd", nil)                                                                        // old by its name
	s3.Put(ImportBucket, RecordKey("20261010-100000-eeeeeeee"), record(t, Record{Kind: "fetch", At: old}), old) // a fetch that copied nothing

	sw := &Sweeper{Store: store, Client: c, Now: func() time.Time { return now }}
	sw.SweepOnce(context.Background())

	want := []string{
		".records/20261010-100000-cccccccc.json", ".records/20261020-110000-bbbbbbbb.json",
		"20261010-100000-cccccccc/manifest.json.age", "20261020-110000-bbbbbbbb/manifest.json.age",
	}
	if got := s3.Keys(ImportBucket); !slices.Equal(got, want) {
		t.Errorf("left %v, want %v", got, want)
	}
}

// A tenant's usage is what its own prefixes hold, or what a fetch still
// copying was said to hold; past MaxTenantImports nothing more is taken, and
// one bundle is never more than MaxTenantUpload.
func TestATenantsAllowanceIsWhatItsLimitsLeave(t *testing.T) {
	s3, store, _ := importsWorld(t)
	now := time.Now()
	s3.Put(ImportBucket, "p1/a.age", make([]byte, 1000), now)
	s3.Put(ImportBucket, RecordKey("p1"), record(t, Record{Tenant: "demo", Kind: "upload", At: now}), now)
	s3.Put(ImportBucket, RecordKey("p2"), record(t, Record{Tenant: "demo", Kind: "fetch", At: now, Size: 30 << 30}), now)
	s3.Put(ImportBucket, "p3/a.age", make([]byte, 5000), now)
	s3.Put(ImportBucket, RecordKey("p3"), record(t, Record{Tenant: "other", Kind: "upload", At: now}), now)

	im, _, err := store.OpenImports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	used, err := im.TenantUsage(context.Background(), "demo")
	if err != nil || used != 30<<30+1000 {
		t.Fatalf("usage %d, %v", used, err)
	}
	left, err := im.TenantAllowance(context.Background(), "demo")
	if err != nil || left != MaxTenantImports-used {
		t.Errorf("allowance %d, %v", left, err)
	}
	if left, err := im.TenantAllowance(context.Background(), "fresh"); err != nil || left != MaxTenantUpload {
		t.Errorf("a tenant with nothing there may put %d, %v", left, err)
	}
	s3.Put(ImportBucket, RecordKey("p4"), record(t, Record{Tenant: "demo", Kind: "fetch", At: now, Size: 20 << 30}), now)
	if _, err := im.TenantAllowance(context.Background(), "demo"); !errors.Is(err, ErrImportLimit) {
		t.Errorf("a tenant at its limit was given more: %v", err)
	}
}

// Removing an upload removes its record with it.
func TestRemovingAnImportRemovesItsRecord(t *testing.T) {
	s3, store, _ := importsWorld(t)
	now := time.Now()
	s3.Put(ImportBucket, "20261010-100000-aaaaaaaa/manifest.json.age", []byte("x"), now)
	s3.Put(ImportBucket, RecordKey("20261010-100000-aaaaaaaa"), record(t, Record{Kind: "upload", At: now}), now)
	if err := store.RemoveImported(context.Background(), gentianov1alpha1.BundleRef{Bucket: ImportBucket, Prefix: "20261010-100000-aaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if got := s3.Keys(ImportBucket); len(got) != 0 {
		t.Errorf("left %v", got)
	}
}
