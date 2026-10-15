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

// An outside realm a tenant's people sign in at reaches the XTenant only
// where the tenant names one, and the XRD and the Composition carry it.
func TestBrokeredToReachesTheXTenantOnlyWhereATenantNamesOne(t *testing.T) {
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
	if _, stated := spec["brokeredTo"]; stated {
		t.Fatalf("a tenant that names no outside realm projects one: %v", spec["brokeredTo"])
	}

	named := plain.DeepCopy()
	named.Spec.BrokeredTo = []gentianov1alpha1.TenantBrokeredTo{{
		Name:        "aluvian",
		RedirectURI: "https://id.aluvian.example/auth/realms/customers/broker/acme/endpoint",
		JWKSURL:     "https://id.aluvian.example/auth/realms/customers/protocol/openid-connect/certs",
	}}
	xr, err = r.buildXTenant(context.Background(), named)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ = xr.Object["spec"].(map[string]interface{})
	list, _ := spec["brokeredTo"].([]interface{})
	if len(list) != 1 || list[0].(map[string]interface{})["jwksURL"] != named.Spec.BrokeredTo[0].JWKSURL {
		t.Fatalf("brokeredTo is not projected: %v", spec["brokeredTo"])
	}

	xrd, err := os.ReadFile("../../crossplane/xrds/tenant.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xrd), "brokeredTo:") {
		t.Error("crossplane/xrds/tenant.yaml does not declare brokeredTo")
	}
	composition, err := os.ReadFile("../../crossplane/compositions/tenant-default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(composition), `dig "brokeredTo" list $spec`) {
		t.Error("tenant-default.yaml does not read brokeredTo")
	}
}
