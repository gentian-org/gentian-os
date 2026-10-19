/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package oidc

import (
	"context"
	"errors"
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
	pack, templates, ok, err := ResolvePack(context.Background(), c, "catalogue-test-app", "catalogue-test-client")
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
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Labels: map[string]string{gentianov1alpha1.LabelProfileName: "app"}},
	}).Build()
	_, _, ok, err := ResolvePack(context.Background(), c, "app", "unknown-client")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected no pack for unknown client")
	}
}

func TestResolvePackRequiresClient(t *testing.T) {
	_, _, _, err := ResolvePack(context.Background(), nil, "catalogue-test-app", "catalogue-test-client")
	if err == nil {
		t.Fatal("expected error when client is nil")
	}
}

// A pack is looked up in the catalogue labelled with the profile, where the
// app Composition looks, and nowhere else: one another profile's catalogue or
// an unlabelled one holds is not the profile's, and the client stays the
// Composition's.
func TestResolvePackReadsOnlyTheProfilesOwnCatalogue(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	catalog := func(name, profile string) *gentianov1alpha1.OIDCPackCatalog {
		c := &gentianov1alpha1.OIDCPackCatalog{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: gentianov1alpha1.OIDCPackCatalogSpec{Packs: map[string]gentianov1alpha1.OIDCPackSpec{
				name + "-client": {ServiceClient: true},
			}},
		}
		if profile != "" {
			c.Labels = map[string]string{gentianov1alpha1.LabelProfileName: profile}
		}
		return c
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		catalog("shop-oidc", "shop"),
		catalog("drive-oidc", "drive"),
		catalog("kernel", ""),
	).Build()
	ctx := context.Background()

	if _, _, ok, err := ResolvePack(ctx, c, "shop", "shop-oidc-client"); err != nil || !ok {
		t.Fatalf("the profile's own pack: ok=%v err=%v", ok, err)
	}
	for _, clientID := range []string{"drive-oidc-client", "kernel-client"} {
		if _, _, ok, err := ResolvePack(ctx, c, "shop", clientID); err != nil || ok {
			t.Errorf("%s resolved for shop: ok=%v err=%v", clientID, ok, err)
		}
	}
	if _, _, ok, err := ResolvePack(ctx, c, "", "kernel-client"); err != nil || ok {
		t.Errorf("a pack resolved for no profile: ok=%v err=%v", ok, err)
	}
}

// Two catalogues labelled with one profile: which the Composition reads is
// not defined, and the operator does not guess.
func TestResolvePackRefusesTwoCataloguesOfOneProfile(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	labelled := map[string]string{gentianov1alpha1.LabelProfileName: "shop"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&gentianov1alpha1.OIDCPackCatalog{ObjectMeta: metav1.ObjectMeta{Name: "shop-oidc", Labels: labelled}},
		&gentianov1alpha1.OIDCPackCatalog{ObjectMeta: metav1.ObjectMeta{Name: "shop-old-oidc", Labels: labelled}},
	).Build()
	_, _, _, err := ResolvePack(context.Background(), c, "shop", "shop")
	if !errors.Is(err, ErrCatalogAmbiguous) {
		t.Fatalf("err = %v, want ErrCatalogAmbiguous", err)
	}
}
