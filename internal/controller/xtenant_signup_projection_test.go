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

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Sign-up passes through the same four places as a realm policy: the Tenant,
// the projection onto the XTenant, the XRD's schema and the Composition. A
// tenant that does not say it projects nothing, so the XTenant of every
// tenant made before the setting existed is what it was.
func TestSignUpReachesTheXTenantOnlyWhereATenantSaysSo(t *testing.T) {
	r := &TenantReconciler{
		Client:       fake.NewClientBuilder().WithScheme(appStoreScheme(t)).Build(),
		KernelDomain: "k.example", TenancyMode: "multi",
	}
	plain := &gentianov1alpha1.Tenant{}
	plain.Name = "acme"
	plain.Spec.DisplayName = "Acme"
	xr, err := r.buildXTenant(context.Background(), plain)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := xr.Object["spec"].(map[string]interface{})
	if _, stated := spec["signUp"]; stated {
		t.Fatalf("a tenant that says nothing about sign-up projects it: %v", spec["signUp"])
	}

	open := plain.DeepCopy()
	open.Spec.SignUp = &gentianov1alpha1.TenantSignUp{Enabled: true}
	xr, err = r.buildXTenant(context.Background(), open)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ = xr.Object["spec"].(map[string]interface{})
	if got, _ := spec["signUp"].(map[string]interface{}); got["enabled"] != true {
		t.Fatalf("sign-up is not projected: %v", spec["signUp"])
	}

	// The XRD declares it, or the API server prunes it in silence; the
	// Composition reads it, or the realm keeps taking no registrations.
	xrd, err := os.ReadFile("../../crossplane/xrds/tenant.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xrd), "signUp:") {
		t.Error("crossplane/xrds/tenant.yaml does not declare signUp")
	}
	composition, err := os.ReadFile("../../crossplane/compositions/tenant-default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(composition), `dig "signUp" "enabled" false $spec`) {
		t.Error("tenant-default.yaml does not read signUp.enabled")
	}
}

// A tenant's partners reach the XTenant with the claim's defaults, within
// the claim's allowed issuers, as and salt filled in; a tenant without any
// and a claim without defaults project none.
func TestPartnersReachTheXTenantWithinTheClaimsBounds(t *testing.T) {
	r := &TenantReconciler{
		Client:       fake.NewClientBuilder().WithScheme(appStoreScheme(t)).Build(),
		KernelDomain: "k.example", TenancyMode: "multi",
	}
	plain := &gentianov1alpha1.Tenant{}
	plain.Name = "acme"
	plain.UID = "0f1e2d3c-aaaa-bbbb-cccc-000000000001"
	plain.Spec.DisplayName = "Acme"
	xr, err := r.buildXTenant(context.Background(), plain)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := xr.Object["spec"].(map[string]interface{})
	if _, stated := spec["partners"]; stated {
		t.Fatalf("a tenant with no partner projects one: %v", spec["partners"])
	}

	claim := clusterPartners{AllowedIssuers: []string{"https://id.shop.example/auth/realms/*"}}
	claim.Defaults = append(claim.Defaults, struct {
		Name   string `json:"name"`
		Issuer string `json:"issuer"`
	}{Name: "aluvian", Issuer: "https://id.aluvian.example/auth/realms/customers"})
	named := plain.DeepCopy()
	named.Spec.Partners = []gentianov1alpha1.TenantPartner{
		{Name: "shop", Issuer: "https://id.shop.example/auth/realms/main", As: "acme", Issue: true},
		{Name: "elsewhere", Issuer: "https://id.other.example/auth/realms/x", Issue: true},
		{Name: "customer", Issuer: "https://id.shop.example/auth/realms/c1",
			Accept: &gentianov1alpha1.TenantPartnerAccept{Accounts: gentianov1alpha1.PartnerAccountsCreate}},
	}
	list := tenantPartnerProjection(named, claim)
	byName := map[string]map[string]interface{}{}
	for _, e := range list {
		m := e.(map[string]interface{})
		byName[m["name"].(string)] = m
	}
	if len(list) != 3 || byName["elsewhere"] != nil {
		t.Fatalf("projected %v; want aluvian, customer and shop, not an issuer the claim does not allow", list)
	}
	if a := byName["aluvian"]; a["issue"] != true || a["as"] != string(plain.UID) || a["salt"] == "" {
		t.Fatalf("the default is projected as %v", a)
	}
	if byName["shop"]["as"] != "acme" {
		t.Fatalf("a stated as is not kept: %v", byName["shop"])
	}
	if acc, _ := byName["customer"]["accept"].(map[string]interface{}); acc["accounts"] != "create" || byName["customer"]["issue"] != nil {
		t.Fatalf("a receiving entry is projected as %v", byName["customer"])
	}
	if byName["aluvian"]["salt"] == byName["shop"]["salt"] {
		t.Fatal("two partners share a salt")
	}
	if tenantPartnerProjection(&gentianov1alpha1.Tenant{Spec: named.Spec}, claim) != nil {
		t.Fatal("a tenant without a uid projects partners")
	}
	if !IssuerAllowed([]string{"https://*"}, "https://id.x.example/auth/realms/y") ||
		IssuerAllowed([]string{"https://id.a.example/auth/realms/b"}, "https://id.a.example/auth/realms/bc") {
		t.Fatal("IssuerAllowed matches wrongly")
	}
	composition, err := os.ReadFile("../../crossplane/compositions/tenant-default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(composition), `dig "partners" list $spec`) {
		t.Error("tenant-default.yaml does not read partners")
	}
}

// A sign-in between two tenants reaches the composites only where both say
// so: the target gets the identity provider with its groups as realm paths
// -- a declared group and the members, never one it does not declare or an
// administrators' group -- and the source gets the client, beside what it
// declares itself. One side alone composes nothing on either.
func TestASignInBetweenTenantsTakesBothAndGivesOnlyDeclaredGroups(t *testing.T) {
	ctx := context.Background()
	team := &gentianov1alpha1.Tenant{}
	team.Name = "team"
	team.Spec.DisplayName = "Aluvian"
	customers := &gentianov1alpha1.Tenant{}
	customers.Name = "customers"
	customers.Spec.DisplayName = "Customers"
	customers.Spec.Groups = []gentianov1alpha1.TenantGroup{{Name: "website-editors"}}
	customers.Spec.SignInFrom = []gentianov1alpha1.TenantSignInFrom{{
		Tenant: "team", Groups: []string{"website-editors", "members", "admins", "not-declared"},
	}}

	build := func(target *gentianov1alpha1.Tenant, objs ...client.Object) map[string]interface{} {
		r := &TenantReconciler{
			Client:       fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(objs...).Build(),
			KernelDomain: "aluvian.io", TenancyMode: "multi",
		}
		xr, err := r.buildXTenant(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		spec, _ := xr.Object["spec"].(map[string]interface{})
		return spec
	}

	// Only the target names the other: nothing on either side.
	if spec := build(customers, team.DeepCopy(), customers.DeepCopy()); spec["signInFrom"] != nil {
		t.Fatalf("a sign-in the source did not consent to: %v", spec["signInFrom"])
	}
	if spec := build(team, team.DeepCopy(), customers.DeepCopy()); spec["brokeredTo"] != nil {
		t.Fatalf("a client for a tenant the source does not name: %v", spec["brokeredTo"])
	}

	consenting := team.DeepCopy()
	consenting.Spec.SignInTo = []string{"customers"}
	spec := build(customers, consenting.DeepCopy(), customers.DeepCopy())
	list, _ := spec["signInFrom"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("signInFrom = %v", spec["signInFrom"])
	}
	entry := list[0].(map[string]interface{})
	if entry["alias"] != "tenant-team" || entry["realm"] != "team" || entry["clientID"] != "broker-to-customers" ||
		entry["displayName"] != "Aluvian" {
		t.Fatalf("entry = %v", entry)
	}
	groups := entry["groups"].([]interface{})
	if len(groups) != 2 || groups[0] != "/website-editors" || groups[1] != "/gentian:tenant:customers:members" {
		t.Fatalf("groups = %v: only a declared group and the members", groups)
	}
	if got := spec["groups"].([]interface{}); len(got) != 1 || got[0] != "website-editors" {
		t.Fatalf("declared groups = %v", got)
	}

	source := build(consenting, consenting.DeepCopy(), customers.DeepCopy())
	clients, _ := source["brokeredTo"].([]interface{})
	if len(clients) != 1 {
		t.Fatalf("brokeredTo = %v", source["brokeredTo"])
	}
	c := clients[0].(map[string]interface{})
	if c["name"] != "customers" ||
		c["redirectURI"] != "https://id.aluvian.io/auth/realms/customers/broker/tenant-team/endpoint" ||
		c["jwksURL"] != "https://id.aluvian.io/auth/realms/customers/protocol/openid-connect/certs" {
		t.Fatalf("client = %v", c)
	}
}
