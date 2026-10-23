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
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The store's sign-in is one brokeredTo client, broker-to-store, sent back to
// the store realm's provider for this tenant's uid, carrying identity only;
// a tenant that has not turned it on projects nothing, and a declared entry
// named store does not stand beside it.
func TestStoreSignInProjectsOneIdentityOnlyClientKeyedByTheUID(t *testing.T) {
	r := &TenantReconciler{
		Client:       fake.NewClientBuilder().WithScheme(appStoreScheme(t)).Build(),
		KernelDomain: "k.example", TenancyMode: "multi",
	}
	plain := &gentianov1alpha1.Tenant{}
	plain.Name = "acme"
	plain.UID = "0f8e7d6c-5b4a-4321-9876-0123456789ab"
	plain.Spec.DisplayName = "Acme"
	xr, err := r.buildXTenant(context.Background(), plain)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := xr.Object["spec"].(map[string]interface{})
	if _, stated := spec["brokeredTo"]; stated {
		t.Fatalf("a tenant without the store's sign-in projects a client: %v", spec["brokeredTo"])
	}

	on := plain.DeepCopy()
	on.Spec.Store = &gentianov1alpha1.TenantStore{SignIn: &gentianov1alpha1.TenantStoreSignIn{
		Issuer: "https://id.store.example/auth/realms/customers",
	}}
	on.Spec.BrokeredTo = []gentianov1alpha1.TenantBrokeredTo{{
		Name: "store", RedirectURI: "https://elsewhere.example/x", JWKSURL: "https://elsewhere.example/k",
	}}
	xr, err = r.buildXTenant(context.Background(), on)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ = xr.Object["spec"].(map[string]interface{})
	list, _ := spec["brokeredTo"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("want the store's client alone, got %v", list)
	}
	got := list[0].(map[string]interface{})
	want := "https://id.store.example/auth/realms/customers/broker/org-" + string(plain.UID) + "/endpoint"
	if got["name"] != "store" || got["redirectURI"] != want || got["identityOnly"] != true ||
		got["jwksURL"] != "https://id.store.example/auth/realms/customers/protocol/openid-connect/certs" {
		t.Fatalf("the store's client: %v", got)
	}

	xrd, err := os.ReadFile("../../crossplane/xrds/tenant.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xrd), "identityOnly:") || !strings.Contains(string(xrd), "linkExisting:") ||
		!strings.Contains(string(xrd), "createAccounts:") {
		t.Error("crossplane/xrds/tenant.yaml does not declare identityOnly, linkExisting and createAccounts")
	}
}
