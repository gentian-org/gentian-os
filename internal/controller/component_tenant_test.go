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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// isolatedTenant runs its workloads in a namespace of its own choosing,
// whose name says nothing about whose it is.
func isolatedTenant() *gentianov1alpha1.Tenant {
	tenant := acmeTenantFixture()
	tenant.Spec.Isolation = &gentianov1alpha1.TenantIsolation{Namespace: "acme-apps"}
	return tenant
}

// A component is its tenant's by the Tenant that runs its workloads in the
// component's namespace, and only one whose Tenant is gone is taken by the
// default namespace's name.
func TestAComponentsTenantIsTheTenantOfItsNamespace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tenant    *gentianov1alpha1.Tenant
		namespace string
		want      string
	}{
		{"default namespace", acmeTenantFixture(), "tenant-acme", "acme"},
		{"a namespace of its own", isolatedTenant(), "acme-apps", "acme"},
		{"tenant gone", nil, "tenant-acme", "acme"},
	} {
		if got := componentTenant(tc.tenant, tc.namespace); got != tc.want {
			t.Errorf("%s: componentTenant = %q, want %q", tc.name, got, tc.want)
		}
		if got := componentLabels(componentFor("wiki", tc.namespace), componentTenant(tc.tenant, tc.namespace))[tenantLabel]; got != tc.want {
			t.Errorf("%s: the tenant label is %q, want %q", tc.name, got, tc.want)
		}
	}
}

// What is rendered for an app of a tenant in a namespace of its own is the
// tenant's: its vault paths and its labels name the tenant, not the
// namespace.
func TestARenderNamesTheTenantAndNotItsNamespace(t *testing.T) {
	scheme := rightsScheme(t)
	profile := chartProfileFixture("oci://x")
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		Cache: &gentianov1alpha1.CacheValueMapping{HostKey: "cache.host"},
	}
	tenant := isolatedTenant()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, profile).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
	comp := componentFor("vendor-app", tenant.NamespaceName())
	comp.Spec.ProfileRef.Name = profile.Name

	in := mustRenderInput(t, r, comp, profile, tenant)
	if in.Tenant() != "acme" || in.Namespace != "acme-apps" {
		t.Fatalf("rendered for tenant %q in %q, want acme in acme-apps", in.Tenant(), in.Namespace)
	}
	objs := renderApp(t, in)
	data, _ := sensitiveOf(t, objs, comp.Name)
	if len(data) == 0 {
		t.Fatal("nothing is read from the vault")
	}
	for _, e := range data {
		key, _, _ := unstructured.NestedString(e, "remoteRef", "key")
		if !strings.HasPrefix(key, "gentian-os/tenants/acme/") {
			t.Errorf("reads %q, want a path under the tenant acme", key)
		}
	}
	for _, obj := range objs {
		if v, ok := obj.GetLabels()[tenantLabel]; ok && v != "acme" {
			t.Errorf("%s %s is labeled for tenant %q", obj.GetKind(), obj.GetName(), v)
		}
	}
}

// The arbiter's table names the tenant whose namespace holds the key, and
// that tenant's realm, wherever the tenant runs its workloads.
func TestTheArbitersTableNamesTheTenantOfANamespaceOfItsOwn(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	tenant := isolatedTenant()
	tenant.Spec.Isolation.KeycloakRealm = "acme-people"
	ledger := rightsComponent("acme", "ledger")
	ledger.Namespace = tenant.NamespaceName()
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant, ledger, authorizationProfile("ledger", ledgerModel)).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	if err := r.ensureAuthorization(ctx, ledger, componentTenant(tenant, ledger.Namespace), authorizationProfile("ledger", ledgerModel)); err != nil {
		t.Fatal(err)
	}
	stores, err := authorizationStores(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(stores) != 1 || stores[0].Tenant != "acme" || stores[0].Realm != "acme-people" {
		t.Fatalf("stores = %+v, want one entry for tenant acme in realm acme-people", stores)
	}
}
