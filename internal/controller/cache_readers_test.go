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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

func externalSecretReading(name, ns, key string) *unstructured.Unstructured {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(secrets.ExternalSecretGVK)
	es.SetName(name)
	es.SetNamespace(ns)
	_ = unstructured.SetNestedSlice(es.Object, []any{
		map[string]any{"secretKey": "cache-host", "remoteRef": map[string]any{"key": key, "property": "host"}},
	}, "spec", "data")
	return es
}

// An app whose cache moved has its ExternalSecret read again at once: the
// one that reads its cache record is stamped with the new address, once, and
// one that reads something else is left alone.
func TestACacheThatMovedHasItsReadersReadAgain(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(secrets.ExternalSecretGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(secrets.ExternalSecretGVK.GroupVersion().WithKind(secrets.ExternalSecretGVK.Kind+"List"), &unstructured.UnstructuredList{})
	path := secrets.CategoryPath("demo", "wiki", "cache")
	reader := externalSecretReading("wiki-sensitive-values", "tenant-demo", path)
	other := externalSecretReading("shop-sensitive-values", "tenant-demo", secrets.CategoryPath("demo", "shop", "cache"))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(reader, other).Build()
	r := &TenantReconciler{Client: c}

	stamp := func(es *unstructured.Unstructured) (string, string) {
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(secrets.ExternalSecretGVK)
		if err := c.Get(ctx, client.ObjectKeyFromObject(es), got); err != nil {
			t.Fatal(err)
		}
		return got.GetAnnotations()[cacheAddressAnnotation], got.GetResourceVersion()
	}

	r.refreshCacheReaders(ctx, "tenant-demo", path, "cache.system-cache.svc.cluster.local:6379")
	address, version := stamp(reader)
	if address != "cache.system-cache.svc.cluster.local:6379" {
		t.Fatalf("the reader of the moved record carries %q", address)
	}
	if address, _ := stamp(other); address != "" {
		t.Fatalf("a reader of another record was stamped: %q", address)
	}

	// The same address again changes nothing: no read is asked for.
	r.refreshCacheReaders(ctx, "tenant-demo", path, "cache.system-cache.svc.cluster.local:6379")
	if _, again := stamp(reader); again != version {
		t.Fatalf("an unchanged address was written again (%s, then %s)", version, again)
	}
}
