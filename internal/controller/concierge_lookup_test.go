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

	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The concierge finds a tenant on a custom domain by the domain's hash
// and nothing else: a tenant without one adds no entry, and the entry sends
// the browser to the console on the custom domain.
func TestTheSignInLookupHoldsOnlyCustomDomainsByTheirHash(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	acme := acmeTenantFixture()
	acme.Status.Domain = "acme.example"
	plain := &gentianov1alpha1.Tenant{}
	plain.Name = "plain"
	platform := platformTenantFixture()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(acme, plain, platform).WithStatusSubresource(acme).Build()
	if err := c.Status().Update(ctx, acme); err != nil {
		t.Fatal(err)
	}
	r := &ConciergeLookupReconciler{Client: c, KernelDomain: "k.example", TenancyMode: "multi"}
	if _, err := r.Reconcile(ctx, conciergeLookupRequest); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	// Beside the concierge, which is the platform tenant's component.
	key := types.NamespacedName{Name: conciergeLookupConfigMap, Namespace: tenantNamespaceName(platform)}
	if err := c.Get(ctx, key, cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.Data) != 1 {
		t.Fatalf("entries = %v, want the custom domain's only", cm.Data)
	}
	// sha256("acme.example"), as the page computes it with WebCrypto.
	want := "54667cc7be6265f6a4cdfe25b9c89d52aea7817c4e570cb678feec57c23f4a6a.json"
	got := conciergeLookupKey("acme.example")
	if got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}
	if cm.Data[got] != `{"url":"https://console.acme.example/"}` {
		t.Fatalf("entry = %q", cm.Data[got])
	}
}

// userTenant is a tenant of the cluster's users, in the phase given.
func userTenant(name string, phase gentianov1alpha1.TenantPhase) gentianov1alpha1.Tenant {
	t := gentianov1alpha1.Tenant{}
	t.Name = name
	t.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: name}
	t.Status.Phase = phase
	return t
}

// Single-tenant is a property of what exists: the platform tenant and exactly
// one user tenant. The page is told so with one file naming that tenant's
// console, and with none or several there is no such file and it asks.
func TestTheBareDomainForwardsOnlyWhileThereIsExactlyOneUserTenant(t *testing.T) {
	ready := gentianov1alpha1.TenantPhaseReady
	platform := *platformTenantFixture()
	platform.Status.Phase = ready
	custom := userTenant("acme", ready)
	custom.Status.Domain = "acme.example"
	deleting := userTenant("gone", ready)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	cases := []struct {
		name    string
		tenants []gentianov1alpha1.Tenant
		want    string
	}{
		{"no tenant at all", nil, ""},
		{"the platform tenant alone is not a single-tenant cluster", []gentianov1alpha1.Tenant{platform}, ""},
		{"one user tenant", []gentianov1alpha1.Tenant{platform, userTenant("acme", ready)},
			`{"url":"https://console.acme.k.example/"}`},
		{"one user tenant and no platform tenant yet", []gentianov1alpha1.Tenant{userTenant("acme", ready)},
			`{"url":"https://console.acme.k.example/"}`},
		{"one user tenant on a custom domain", []gentianov1alpha1.Tenant{platform, custom},
			`{"url":"https://console.acme.example/"}`},
		{"two user tenants", []gentianov1alpha1.Tenant{platform, userTenant("acme", ready), userTenant("beta", ready)}, ""},
		{"one user tenant that is not Ready yet", []gentianov1alpha1.Tenant{platform, userTenant("acme", gentianov1alpha1.TenantPhaseProvisioning)}, ""},
		{"a second tenant still being provisioned already ends it",
			[]gentianov1alpha1.Tenant{platform, userTenant("acme", ready), userTenant("beta", gentianov1alpha1.TenantPhaseProvisioning)}, ""},
		{"a tenant being deleted is not counted",
			[]gentianov1alpha1.Tenant{platform, userTenant("acme", ready), deleting},
			`{"url":"https://console.acme.k.example/"}`},
		{"the only user tenant being deleted", []gentianov1alpha1.Tenant{platform, deleting}, ""},
	}
	for _, c := range cases {
		got := conciergeLookupData(c.tenants, "k.example", "kernel", "multi")[conciergeSingleKey]
		if got != c.want {
			t.Errorf("%s: %s = %q, want %q", c.name, conciergeSingleKey, got, c.want)
		}
	}
}

// The tenancy mode decides nothing about it. A cluster in the older single
// mode carries the platform tenant alone, which is not a user tenant, so its
// bare domain forwards nowhere; and no tenant is ever forwarded to the
// kernel's own console, which is the administrators'.
func TestTheTenancyModeDoesNotMakeAClusterSingleTenant(t *testing.T) {
	platform := *platformTenantFixture()
	platform.Status.Phase = gentianov1alpha1.TenantPhaseReady
	if got, ok := conciergeLookupData([]gentianov1alpha1.Tenant{platform}, "k.example", "kernel", "single")[conciergeSingleKey]; ok {
		t.Fatalf("the platform tenant alone was named as the one console: %q", got)
	}
	// Under the single mode every tenant's domain is the kernel domain.
	flat := []gentianov1alpha1.Tenant{platform, userTenant("acme", gentianov1alpha1.TenantPhaseReady)}
	if got, ok := conciergeLookupData(flat, "k.example", "kernel", "single")[conciergeSingleKey]; ok {
		t.Fatalf("a tenant on the kernel domain was forwarded to the administrators' console: %q", got)
	}
}

// The forward starts and stops by itself: each reconcile re-derives the
// file from the tenants there are.
func TestTheForwardFollowsTheTenants(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	platform := platformTenantFixture()
	acme := userTenant("acme", gentianov1alpha1.TenantPhaseReady)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(platform, &acme).
		WithStatusSubresource(&gentianov1alpha1.Tenant{}).Build()
	ready := func(name string) {
		t.Helper()
		obj := &gentianov1alpha1.Tenant{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
			t.Fatal(err)
		}
		obj.Status.Phase = gentianov1alpha1.TenantPhaseReady
		if err := c.Status().Update(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	ready("acme")
	r := &ConciergeLookupReconciler{Client: c, KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	single := func() (string, bool) {
		t.Helper()
		if _, err := r.Reconcile(ctx, conciergeLookupRequest); err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Name: conciergeLookupConfigMap, Namespace: tenantNamespaceName(platform)}, cm); err != nil {
			t.Fatal(err)
		}
		v, ok := cm.Data[conciergeSingleKey]
		return v, ok
	}
	if v, _ := single(); v != `{"url":"https://console.acme.k.example/"}` {
		t.Fatalf("one user tenant: %s = %q", conciergeSingleKey, v)
	}
	beta := userTenant("beta", "")
	if err := c.Create(ctx, &beta); err != nil {
		t.Fatal(err)
	}
	if v, ok := single(); ok {
		t.Fatalf("a second tenant exists and the bare domain still forwards: %q", v)
	}
	if err := c.Delete(ctx, &beta); err != nil {
		t.Fatal(err)
	}
	if v, _ := single(); v != `{"url":"https://console.acme.k.example/"}` {
		t.Fatalf("back to one user tenant: %s = %q", conciergeSingleKey, v)
	}
	gone := &gentianov1alpha1.Tenant{}
	gone.Name = "acme"
	if err := c.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if v, ok := single(); ok {
		t.Fatalf("no user tenant is left and the bare domain still forwards: %q", v)
	}
}

// The edge treats the bare domain the same however many tenants there are:
// the kernel routes nothing on it (it is the concierge's, published from the
// platform tenant's DMZ), www is sent to it, and never to a console. Which
// console a visitor ends at is the lookup's one file, tested above; a change
// in the number of tenants therefore reprograms no route on the bare domain.
func TestTheKernelRoutesNothingOnTheBareDomainHoweverManyTenants(t *testing.T) {
	const kernel = "k.example"
	for _, names := range [][]string{nil, {"acme"}, {"acme", "beta"}} {
		domains := make([]string, 0, len(names))
		for _, n := range names {
			domains = append(domains, n+"."+kernel)
		}
		specs := kernelHTTPRouteSpecs(kernel, domains, nil, names, false, "c", true, true)
		var www string
		apexes := map[string]string{}
		for _, spec := range specs {
			if spec.host == kernel {
				t.Fatalf("%d user tenants: the kernel routes the bare domain itself: %s", len(names), spec.name)
			}
			switch {
			case spec.name == kernelRouteWWWRedirect:
				www = string(*spec.rules[0].Filters[0].RequestRedirect.Hostname)
			case strings.HasPrefix(spec.name, "tenant-") && strings.HasSuffix(spec.name, "-apex"):
				apexes[spec.host] = string(*spec.rules[0].Filters[0].RequestRedirect.Hostname)
			}
		}
		if www != kernel {
			t.Fatalf("%d user tenants: www -> %q, want the bare domain", len(names), www)
		}
		// A tenant's own apex leads to that tenant's console, in every case.
		if len(apexes) != len(names) {
			t.Fatalf("%d user tenants: apex routes = %v", len(names), apexes)
		}
		for _, d := range domains {
			if apexes[d] != "console."+d {
				t.Fatalf("apex of %s -> %q", d, apexes[d])
			}
		}
	}
}
