/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore"
	"github.com/gentian-org/gentian-os/internal/bundlestore/s3test"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func sourcesService(t *testing.T, objects ...client.Object) (*Service, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &Service{client: c}, c
}

// clusterStorage is the cluster's own storage, in memory, and the Secret
// that holds its address and keys.
func clusterStorage(t *testing.T) (*s3test.Server, *corev1.Secret) {
	t.Helper()
	s3 := s3test.New(t)
	return s3, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backup.StorageAdminSecret, Namespace: layout.System("s3")},
		Data: map[string][]byte{"endpoint": []byte(s3.URL),
			backup.DestinationAccessKeyField: []byte("a"), backup.DestinationSecretKeyField: []byte("b")},
	}
}

func oldStore() *gentianov1alpha1.BundleSource {
	return &gentianov1alpha1.BundleSource{
		ObjectMeta: metav1.ObjectMeta{Name: "old-store"},
		Spec: gentianov1alpha1.BundleSourceSpec{
			Endpoint: "https://203.0.113.7:9000", Bucket: "bundles", Prefix: "corp/",
			Region: "ch-gva-2", AddressingStyle: gentianov1alpha1.S3AddressingVirtualHost,
		},
	}
}

// A bundle is named by its source and its prefix there; the platform finds
// the address and the Secret, and the caller names neither.
func TestASourceReferenceResolvesToTheSourcesAddressAndItsOwnKeys(t *testing.T) {
	s, _ := sourcesService(t, oldStore())
	ref, err := s.ResolveSource(context.Background(), SourceRef{Name: "old-store", Prefix: "export-20261001"})
	if err != nil {
		t.Fatal(err)
	}
	want := gentianov1alpha1.BundleRef{
		Bucket: "bundles", Prefix: "corp/export-20261001", Endpoint: "https://203.0.113.7:9000",
		Region: "ch-gva-2", AddressingStyle: gentianov1alpha1.S3AddressingVirtualHost,
		CredentialSecret: backup.SourceCredentialName("old-store"),
	}
	if ref != want {
		t.Errorf("resolved to %+v, want %+v", ref, want)
	}
}

// A prefix is one bundle directly below the source: nothing above it, and
// nothing that walks out of it.
func TestASourceReferenceIsOneBundleBelowTheSource(t *testing.T) {
	s, _ := sourcesService(t, oldStore())
	for _, prefix := range []string{"", ".", "..", "../x", "a/b", "/abs", "x y"} {
		if _, err := s.ResolveSource(context.Background(), SourceRef{Name: "old-store", Prefix: prefix}); err == nil {
			t.Errorf("prefix %q was resolved", prefix)
		}
	}
	if _, err := s.ResolveSource(context.Background(), SourceRef{Name: "nowhere", Prefix: "x"}); !errors.Is(err, errNoSuchSource) {
		t.Errorf("an undeclared source answered %v", err)
	}
}

// A tenant's own sources are its bucket in the cluster's storage and its
// policy's destination, read with the policy's keys; nothing else.
func TestATenantsSourcesAreWhereItsBackupsGo(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "corp"}}
	policy := &gentianov1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "corp"},
		Spec: gentianov1alpha1.BackupPolicySpec{Scope: "tenant", Tenant: "corp",
			Destination: &gentianov1alpha1.BackupDestination{Endpoint: "https://sos.example", Bucket: "corp-bundles"}},
	}
	s, _ := sourcesService(t, tenant, policy)
	views, err := s.TenantSources(context.Background(), "corp")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].Name != TenantSourceStorage || views[1].Name != TenantSourceDestination {
		t.Fatalf("sources %+v", views)
	}
	ref, err := s.ResolveSource(context.Background(), SourceRef{Tenant: "corp", Name: TenantSourceDestination, Prefix: "nightly-1"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Endpoint != "https://sos.example" || ref.Bucket != "corp-bundles" || ref.Prefix != "nightly-1" ||
		ref.CredentialSecret != backup.DestinationSecretName("tenant", "corp") {
		t.Errorf("the destination resolved to %+v", ref)
	}
	if _, err := s.ResolveSource(context.Background(), SourceRef{Tenant: "corp", Name: "old-store", Prefix: "x"}); !errors.Is(err, errNoSuchSource) {
		t.Errorf("a tenant reached a source that is not its own: %v", err)
	}
}

// A fetch is made suspended, confined, and only then let run; its way out
// is owned by it and names the source's address alone.
func TestAFetchRunsOnlyOnceItIsConfinedToItsSource(t *testing.T) {
	s3, secret := clusterStorage(t)
	s, c := sourcesService(t, oldStore(), secret)
	st, err := s.StartFetch(context.Background(), SourceRef{Name: "old-store", Prefix: "export-20261001"}, "ada", "")
	if err != nil {
		t.Fatal(err)
	}
	ns := layout.System("s3")
	var job batchv1.Job
	if err := c.Get(context.Background(), types.NamespacedName{Name: st.Name, Namespace: ns}, &job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.Suspend == nil || *job.Spec.Suspend {
		t.Error("the fetch was left suspended")
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Name: backup.FetchPolicyName(st.Name), Namespace: ns}, &np); err != nil {
		t.Fatalf("the fetch has no way out of its own: %v", err)
	}
	if len(np.OwnerReferences) != 1 || np.OwnerReferences[0].Name != job.Name {
		t.Errorf("the way out is not the fetch's: %+v", np.OwnerReferences)
	}
	if got := np.Spec.Egress[0].To[0].IPBlock.CIDR; got != "203.0.113.7/32" {
		t.Errorf("the way out reaches %s", got)
	}
	if st.Bundle.Bucket != bundlestore.ImportBucket || !importPrefix.MatchString(st.Bundle.Prefix) {
		t.Errorf("the fetch goes to %+v", st.Bundle)
	}
	if !bundlestore.IsImported(&st.Bundle) {
		t.Error("a fetched bundle is not one the import storage holds")
	}

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	got, err := s.Fetch(context.Background(), st.Name, "")
	if err != nil || got.Phase != "ready" || got.Source.Name != "old-store" || got.RequestedBy != "ada" {
		t.Errorf("fetch %+v, %v", got, err)
	}
	// Recorded when it was asked for, as no tenant's, for the sweep.
	var rec bundlestore.Record
	if err := json.Unmarshal(s3.Get(bundlestore.ImportBucket, bundlestore.RecordKey(st.Bundle.Prefix)), &rec); err != nil ||
		rec.Kind != "fetch" || rec.Tenant != "" || rec.At.IsZero() {
		t.Errorf("record %+v, %v", rec, err)
	}
	if _, err := s.Fetch(context.Background(), st.Name, "demo"); err == nil {
		t.Error("a tenant's administrator was shown the cluster's fetch")
	}
}

// A fetch of a tenant's bundle in the cluster's own storage opens no way out
// of the cluster at all.
func TestAFetchFromTheClustersOwnStorageHasNoWayOut(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "corp"}}
	_, secret := clusterStorage(t)
	s, c := sourcesService(t, tenant, secret)
	st, err := s.StartFetch(context.Background(), SourceRef{Tenant: "corp", Name: TenantSourceStorage, Prefix: "nightly-1"}, "ada", "")
	if err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Name: backup.FetchPolicyName(st.Name), Namespace: layout.System("s3")}, &np); err != nil {
		t.Fatal(err)
	}
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				t.Errorf("a fetch inside the cluster may reach %s", peer.IPBlock.CIDR)
			}
		}
	}
}

// Restoring a tenant's source into another tenant is refused before
// anything is made.
func TestATenantsSourceIsRestoredIntoThatTenantAlone(t *testing.T) {
	s, _ := sourcesService(t,
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "corp"}},
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "other"}})
	_, err := s.StartRestore(context.Background(), "other", RestoreRequest{
		Source:     &SourceRef{Tenant: "corp", Name: TenantSourceStorage, Prefix: "nightly-1"},
		Decryption: Decryption{Passphrase: "x"},
	}, "ada")
	if err == nil || !strings.Contains(err.Error(), "alone") {
		t.Errorf("a source of corp was restored into other: %v", err)
	}
}

// A tenant's administrator fetches from the tenant's own sources alone, only
// a bundle whose header names the tenant, and only while it fits in what the
// tenant may still put into the import storage; the copy is recorded as the
// tenant's, and its fetch, and the copy, are the tenant's to see alone.
func TestATenantsAdministratorFetchesOnlyTheTenantsOwnBundlesWithinItsLimit(t *testing.T) {
	ctx := context.Background()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "corp"}}
	s3, secret := clusterStorage(t)
	s, _ := sourcesService(t, tenant, oldStore(), secret)
	bucket := backup.BackupBucket(tenant)
	now := time.Now()
	s3.Put(bucket, "nightly-1/bundle-info.json", []byte(`{"tenant":"corp"}`), now)
	s3.Put(bucket, "nightly-1/manifest.json.age", make([]byte, 2048), now)
	s3.Put(bucket, "stray/bundle-info.json", []byte(`{"tenant":"acme"}`), now)

	if _, err := s.StartFetch(ctx, SourceRef{Name: "old-store", Prefix: "x"}, "eve", "corp"); !errors.Is(err, errNoSuchSource) {
		t.Errorf("a tenant fetched from the cluster's source: %v", err)
	}
	if _, err := s.StartFetch(ctx, SourceRef{Tenant: "corp", Name: TenantSourceStorage, Prefix: "stray"}, "eve", "corp"); !errors.Is(err, errNotTenants) {
		t.Errorf("a tenant fetched a bundle of another: %v", err)
	}
	st, err := s.StartFetch(ctx, SourceRef{Tenant: "corp", Name: TenantSourceStorage, Prefix: "nightly-1"}, "eve", "corp")
	if err != nil {
		t.Fatal(err)
	}
	var rec bundlestore.Record
	if err := json.Unmarshal(s3.Get(bundlestore.ImportBucket, bundlestore.RecordKey(st.Bundle.Prefix)), &rec); err != nil ||
		rec.Tenant != "corp" || rec.Size != 2048+int64(len(`{"tenant":"corp"}`)) {
		t.Errorf("record %+v, %v", rec, err)
	}
	if _, err := s.Fetch(ctx, st.Name, "corp"); err != nil {
		t.Errorf("the tenant cannot see its own fetch: %v", err)
	}
	if _, err := s.Fetch(ctx, st.Name, "acme"); err == nil {
		t.Error("another tenant saw the fetch")
	}
	s3.Put(bundlestore.ImportBucket, st.Bundle.Prefix+"/manifest.json.age", []byte("x"), now)
	if _, err := s.importedRef(ctx, st.Bundle.Prefix, "corp"); err != nil {
		t.Errorf("the tenant cannot read its own copy: %v", err)
	}
	if _, err := s.importedRef(ctx, st.Bundle.Prefix, "acme"); !errors.Is(err, errNoImported) {
		t.Errorf("another tenant reads the copy: %v", err)
	}

	// Past the tenant's total, refused before anything is made.
	s3.Put(bundlestore.ImportBucket, bundlestore.RecordKey("20261001-000000-ffffffff"),
		[]byte(`{"tenant":"corp","kind":"fetch","at":"2026-10-01T00:00:00Z","size":`+strconv.FormatInt(bundlestore.MaxTenantImports, 10)+`}`), now)
	if _, err := s.StartFetch(ctx, SourceRef{Tenant: "corp", Name: TenantSourceStorage, Prefix: "nightly-1"}, "eve", "corp"); !errors.Is(err, bundlestore.ErrImportLimit) {
		t.Errorf("a fetch past the limit was started: %v", err)
	}
}

// A tenant's upload is recorded as its own and refused, with what was
// stored of it removed, once it is larger than the tenant may still put
// into the import storage; the cluster's administrator's is not bounded.
func TestATenantsUploadIsBoundedByItsLimit(t *testing.T) {
	ctx := context.Background()
	s3, secret := clusterStorage(t)
	s, _ := sourcesService(t, secret)
	bundle := func(sizes ...int) *bytes.Buffer {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for i, n := range sizes {
			_ = tw.WriteHeader(&tar.Header{Name: "f" + strconv.Itoa(i) + ".age", Mode: 0o644, Size: int64(n)})
			_, _ = tw.Write(make([]byte, n))
		}
		_ = tw.Close()
		return &buf
	}
	ref, err := s.UploadBundle(ctx, bundle(10), "corp")
	if err != nil {
		t.Fatal(err)
	}
	var rec bundlestore.Record
	if err := json.Unmarshal(s3.Get(bundlestore.ImportBucket, bundlestore.RecordKey(ref.Prefix)), &rec); err != nil || rec.Tenant != "corp" || rec.Kind != "upload" {
		t.Errorf("record %+v, %v", rec, err)
	}
	// Leave the tenant 100 bytes.
	s3.Put(bundlestore.ImportBucket, bundlestore.RecordKey("20261001-000000-ffffffff"),
		[]byte(`{"tenant":"corp","kind":"fetch","at":"2026-10-01T00:00:00Z","size":`+strconv.FormatInt(bundlestore.MaxTenantImports-110, 10)+`}`), time.Now())
	before := len(s3.Keys(bundlestore.ImportBucket))
	if _, err := s.UploadBundle(ctx, bundle(60, 60), "corp"); !errors.Is(err, bundlestore.ErrImportLimit) {
		t.Fatalf("an upload past the limit was taken: %v", err)
	}
	if got := len(s3.Keys(bundlestore.ImportBucket)); got != before {
		t.Errorf("a refused upload left %d objects behind", got-before)
	}
	if _, err := s.UploadBundle(ctx, bundle(60, 60), ""); err != nil {
		t.Errorf("the cluster's administrator's upload was bounded: %v", err)
	}
}

// cut is a body that ends in an error after n bytes: a transfer that stopped.
type cut struct {
	r io.Reader
	n int
}

func (c *cut) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errors.New("connection reset")
	}
	if len(p) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= n
	return n, err
}

// An upload started and filled in steps survives a transfer that stops
// part-way: what arrived stays, the upload stays incomplete and nothing
// restores from it, and sending the bundle again stores only what is
// missing. Once complete, or for another tenant, it is not filled.
func TestAnUploadThatStoppedIsContinued(t *testing.T) {
	ctx := context.Background()
	s3, secret := clusterStorage(t)
	s, _ := sourcesService(t, secret)
	bundle := func(fill byte) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, name := range []string{"manifest.json.age", "volumes/data.tar.gz.age"} {
			_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 4096})
			_, _ = tw.Write(bytes.Repeat([]byte{fill}, 4096))
		}
		_ = tw.Close()
		return buf.Bytes()
	}

	ref, err := s.StartUpload(ctx, "corp")
	if err != nil {
		t.Fatal(err)
	}
	// The first file and part of the second arrive.
	first := bundle('a')
	if _, err := s.FillUpload(ctx, &cut{r: bytes.NewReader(first), n: 512 + 4096 + 512 + 1000}, "corp", ref.Prefix); err == nil {
		t.Fatal("a stopped transfer completed the upload")
	}
	files, complete, err := s.UploadedFiles(ctx, "corp", ref.Prefix)
	if err != nil || complete || len(files) != 1 || files["manifest.json.age"] != 4096 {
		t.Fatalf("stored after the stop: %v, complete %v, %v", files, complete, err)
	}
	if err := s.uploadComplete(ctx, ref); err == nil {
		t.Error("an incomplete upload is restored from")
	}

	// Sent again: the file that arrived is not stored a second time.
	if _, err := s.FillUpload(ctx, bytes.NewReader(bundle('b')), "corp", ref.Prefix); err != nil {
		t.Fatal(err)
	}
	if got := s3.Get(bundlestore.ImportBucket, ref.Prefix+"/manifest.json.age"); !bytes.Equal(got, bytes.Repeat([]byte{'a'}, 4096)) {
		t.Error("a file that had arrived was stored again")
	}
	if got := s3.Get(bundlestore.ImportBucket, ref.Prefix+"/volumes/data.tar.gz.age"); len(got) != 4096 {
		t.Errorf("the missing file is %d bytes", len(got))
	}
	if err := s.uploadComplete(ctx, ref); err != nil {
		t.Errorf("the complete upload is refused: %v", err)
	}
	if _, complete, err := s.UploadedFiles(ctx, "corp", ref.Prefix); err != nil || !complete {
		t.Errorf("the complete upload is listed as complete %v, %v", complete, err)
	}

	if _, err := s.FillUpload(ctx, bytes.NewReader(first), "corp", ref.Prefix); !errors.Is(err, errNoUpload) {
		t.Errorf("a complete upload was filled again: %v", err)
	}
	other, err := s.StartUpload(ctx, "corp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FillUpload(ctx, bytes.NewReader(first), "solo", other.Prefix); !errors.Is(err, errNoUpload) {
		t.Errorf("another tenant filled corp's upload: %v", err)
	}
	if _, _, err := s.UploadedFiles(ctx, "", other.Prefix); !errors.Is(err, errNoUpload) {
		t.Errorf("the cluster's route read a tenant's upload: %v", err)
	}
}
