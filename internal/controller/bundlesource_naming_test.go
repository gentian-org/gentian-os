/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant's store is named <tenant>.<label>, and the cluster's carry no dot:
// no tenant can declare a store under another tenant's name or the
// cluster's, and two stores can never be one object. Tested against a real
// API server, for the reason the backup policy's name is.
func TestABundleSourcesNameSaysWhoseItIs(t *testing.T) {
	ctx := context.Background()
	source := func(name, tenant string) *gentianov1alpha1.BundleSource {
		return &gentianov1alpha1.BundleSource{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: gentianov1alpha1.BundleSourceSpec{
				Tenant: tenant, Endpoint: "https://s3.example", Bucket: "bundles",
			},
		}
	}
	cases := []struct {
		name     string
		obj      *gentianov1alpha1.BundleSource
		admitted bool
	}{
		{"the cluster's, a label", source("old-store", ""), true},
		{"the cluster's, with a dot", source("acme.offsite", ""), false},
		{"a tenant's, under its own name", source("acme.offsite", "acme"), true},
		{"a tenant's, under another tenant's name", source("corp.offsite", "acme"), false},
		{"a tenant's, without its name", source("offsite", "acme"), false},
		{"a tenant's, with a second dot", source("acme.off.site", "acme"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := testClient.Create(ctx, tc.obj)
			if tc.admitted {
				if err != nil {
					t.Fatalf("rejected a correctly named source: %v", err)
				}
				t.Cleanup(func() { _ = testClient.Delete(ctx, tc.obj) })
				return
			}
			if err == nil {
				_ = testClient.Delete(ctx, tc.obj)
				t.Fatal("admitted a source whose name does not say whose it is")
			}
			if !strings.Contains(err.Error(), "a tenant's own is <tenant>.<label>") {
				t.Errorf("rejected, but not by the naming rule: %v", err)
			}
		})
	}
}

// mode: source names a store and nothing else of a destination; a store
// named under any other mode is refused rather than ignored.
func TestAnExportToAStoreNamesTheStoreAlone(t *testing.T) {
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-storetest"}}
	if err := testClient.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, ns) })
	export := func(name string, d gentianov1alpha1.ExportDestination) *gentianov1alpha1.TenantExport {
		return &gentianov1alpha1.TenantExport{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
			Spec:       gentianov1alpha1.TenantExportSpec{Destination: &d},
		}
	}
	source := gentianov1alpha1.ExportDestinationSource
	cases := []struct {
		name     string
		obj      *gentianov1alpha1.TenantExport
		admitted bool
	}{
		{"a store", export("to-store", gentianov1alpha1.ExportDestination{Mode: source, Source: "storetest.offsite"}), true},
		{"no store", export("no-store", gentianov1alpha1.ExportDestination{Mode: source}), false},
		{"a store and a bucket", export("store-bucket", gentianov1alpha1.ExportDestination{Mode: source, Source: "storetest.offsite", Bucket: "other"}), false},
		{"a store and an endpoint", export("store-endpoint", gentianov1alpha1.ExportDestination{Mode: source, Source: "storetest.offsite", Endpoint: "https://s3.example"}), false},
		{"a store under another mode", export("platform-store", gentianov1alpha1.ExportDestination{Mode: gentianov1alpha1.ExportDestinationPlatform, Source: "storetest.offsite"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := testClient.Create(ctx, tc.obj)
			if tc.admitted {
				if err != nil {
					t.Fatalf("rejected an export to a store: %v", err)
				}
				t.Cleanup(func() { _ = testClient.Delete(ctx, tc.obj) })
				return
			}
			if err == nil {
				_ = testClient.Delete(ctx, tc.obj)
				t.Fatal("admitted a destination that mixes a store with something else")
			}
		})
	}
}
