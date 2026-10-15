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
	"errors"
	"strings"
	"testing"

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
	s, c := sourcesService(t, oldStore())
	st, err := s.StartFetch(context.Background(), SourceRef{Name: "old-store", Prefix: "export-20261001"}, "ada")
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
	got, err := s.Fetch(context.Background(), st.Name)
	if err != nil || got.Phase != "ready" || got.Source.Name != "old-store" || got.RequestedBy != "ada" {
		t.Errorf("fetch %+v, %v", got, err)
	}
}

// A fetch of a tenant's bundle in the cluster's own storage opens no way out
// of the cluster at all.
func TestAFetchFromTheClustersOwnStorageHasNoWayOut(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "corp"}}
	s, c := sourcesService(t, tenant)
	st, err := s.StartFetch(context.Background(), SourceRef{Tenant: "corp", Name: TenantSourceStorage, Prefix: "nightly-1"}, "ada")
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
