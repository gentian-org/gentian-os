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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An export writes to a store only when the store is the tenant's own and
// its keys are there; anything else is refused by name before an app is
// paused.
func TestAnExportWritesOnlyToTheTenantsOwnReadyStore(t *testing.T) {
	s := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	store := func(name, tenant string, ready bool) *gentianov1alpha1.BundleSource {
		return &gentianov1alpha1.BundleSource{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       gentianov1alpha1.BundleSourceSpec{Tenant: tenant, Endpoint: "https://s3.example", Bucket: "b"},
			Status:     gentianov1alpha1.BundleSourceStatus{CredentialSatisfied: ready},
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		store("demo.offsite", "demo", true),
		store("demo.waiting", "demo", false),
		store("acme.offsite", "acme", true),
		store("old-store", "", true),
	).Build()
	r := &TenantExportReconciler{Client: c, Scheme: s}
	demo := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	ctx := context.Background()

	got, why, err := r.exportStore(ctx, demo, "demo.offsite")
	if err != nil || why != "" || got == nil || got.Name != "demo.offsite" {
		t.Fatalf("the tenant's own store: %+v, %q, %v", got, why, err)
	}
	for name, want := range map[string]string{
		"demo.waiting": "are not set",
		"acme.offsite": "is not tenant demo's own",
		"old-store":    "is not tenant demo's own",
		"demo.nowhere": "no store",
	} {
		_, why, err := r.exportStore(ctx, demo, name)
		if err != nil || !strings.Contains(why, want) {
			t.Errorf("%s: %q, %v; want a refusal saying %q", name, why, err, want)
		}
	}
}

// An export to a store that declares keys, naming none of its own, is
// encrypted to the store's keys in place of the cluster's; one that names its
// own keeps them; any other destination takes the cluster's.
func TestAStoresKeysReadWhatIsExportedToIt(t *testing.T) {
	t.Setenv(backupRecipientsEnv, "age1clusterkeyclusterkeyclusterkeyclusterkeyclusterkeyclust")
	s := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	tenantKey := "age1qpzry9x8gf2tvdw0s3jn54khce6mua7lqpzry9x8gf2tvdw0s3jn54khce"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&gentianov1alpha1.BundleSource{
			ObjectMeta: metav1.ObjectMeta{Name: "demo.offsite"},
			Spec: gentianov1alpha1.BundleSourceSpec{Tenant: "demo", Endpoint: "https://s3.example", Bucket: "b",
				Recipients: []string{tenantKey}},
		},
		&gentianov1alpha1.BundleSource{
			ObjectMeta: metav1.ObjectMeta{Name: "acme.offsite"},
			Spec: gentianov1alpha1.BundleSourceSpec{Tenant: "acme", Endpoint: "https://s3.example", Bucket: "b",
				Recipients: []string{tenantKey}},
		},
	).Build()
	r := &TenantExportReconciler{Client: c, Scheme: s}
	export := func(d *gentianov1alpha1.ExportDestination, e *gentianov1alpha1.ExportEncryption) *gentianov1alpha1.TenantExport {
		return &gentianov1alpha1.TenantExport{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "tenant-demo"},
			Spec:       gentianov1alpha1.TenantExportSpec{Destination: d, Encryption: e},
		}
	}
	toStore := &gentianov1alpha1.ExportDestination{Mode: gentianov1alpha1.ExportDestinationSource, Source: "demo.offsite"}
	ctx := context.Background()

	enc, err := r.resolveEncryption(ctx, export(toStore, nil))
	if err != nil || strings.Join(enc.Recipients, ",") != tenantKey {
		t.Fatalf("to the store: %v, %v; want the store's key alone", enc.Recipients, err)
	}
	own := "age1ownkeyownkeyownkeyownkeyownkeyownkeyownkeyownkeyownkeyo"
	enc, err = r.resolveEncryption(ctx, export(toStore, &gentianov1alpha1.ExportEncryption{Recipients: []string{own}}))
	if err != nil || strings.Join(enc.Recipients, ",") != own {
		t.Fatalf("naming its own: %v, %v", enc.Recipients, err)
	}
	enc, err = r.resolveEncryption(ctx, export(&gentianov1alpha1.ExportDestination{Mode: gentianov1alpha1.ExportDestinationPlatform}, nil))
	if err != nil || len(enc.Recipients) != 1 || enc.Recipients[0] == tenantKey {
		t.Fatalf("to the cluster's storage: %v, %v; want the cluster's key", enc.Recipients, err)
	}
	if _, err := r.resolveEncryption(ctx, export(&gentianov1alpha1.ExportDestination{
		Mode: gentianov1alpha1.ExportDestinationSource, Source: "acme.offsite"}, nil)); err == nil {
		t.Fatal("another tenant's store gave its keys")
	}
}
