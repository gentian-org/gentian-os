// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package oidc

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func testGentianCatalogClient(t *testing.T) client.Client {
	t.Helper()
	return NewTestClientWithCatalogFile(t, "testdata/minimal-oidc-catalog.yaml")
}

func TestManagedByAttributeGroupNames_FromCatalog(t *testing.T) {
	c := testGentianCatalogClient(t)
	groups, err := ManagedByAttributeGroupNames(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"TestApp",
		"TestAppAdmin",
	}
	if len(groups) != len(want) {
		t.Fatalf("groups: %v want %v", groups, want)
	}
	for i, name := range want {
		if groups[i] != name {
			t.Fatalf("groups[%d]=%q want %q (full: %v)", i, groups[i], name, groups)
		}
	}
}

func TestNormalizeMBAGroupName(t *testing.T) {
	if got := NormalizeMBAGroupName("managed-by-attribute-Fileshare"); got != "Fileshare" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeMBAGroupName("FileshareAdmin"); got != "FileshareAdmin" {
		t.Fatalf("got %q", got)
	}
}

func TestResolvePackClusterCatalog(t *testing.T) {
	c := testGentianCatalogClient(t)
	pack, templates, ok, err := ResolvePack(context.Background(), c, "catalogue-test-client")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected pack from cluster catalog")
	}
	if pack.ScopeName != "catalogue-test-client-scope" {
		t.Fatalf("scopeName: %q", pack.ScopeName)
	}
	if _, found := templates["gentian_useruuid"]; !found {
		t.Fatal("missing mapper template")
	}
}

func TestResolvePackNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&gentianov1alpha1.OIDCPackCatalog{
		ObjectMeta: metav1.ObjectMeta{Name: "empty"},
	}).Build()
	_, _, ok, err := ResolvePack(context.Background(), c, "unknown-client")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected no pack for unknown client")
	}
}

func TestResolvePackRequiresClient(t *testing.T) {
	_, _, _, err := ResolvePack(context.Background(), nil, "catalogue-test-client")
	if err == nil {
		t.Fatal("expected error when client is nil")
	}
}
