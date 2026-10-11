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

	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The concierge finds a tenant on a custom domain by the domain's hash
// and nothing else: a tenant without one adds no entry, and the entry sends
// the browser to the desktop on the custom domain.
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
	if cm.Data[got] != `{"url":"https://desktop.acme.example/"}` {
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

// The lookup forwards nobody. Whether the bare domain asks for an address or
// leads to the one user tenant's desktop is the cluster's tenancy mode, and
// the edge does it (kernelFrontDoor): however many tenants there are and
// whatever the mode, no file here names "the one desktop".
func TestTheLookupNeverNamesAConsoleToForwardTo(t *testing.T) {
	ready := gentianov1alpha1.TenantPhaseReady
	platform := *platformTenantFixture()
	platform.Status.Phase = ready
	for _, mode := range []string{"multi", "single"} {
		for _, tenants := range [][]gentianov1alpha1.Tenant{
			{platform},
			{platform, userTenant("user", ready)},
			{platform, userTenant("acme", ready)},
			{platform, userTenant("acme", ready), userTenant("beta", ready)},
		} {
			if data := conciergeLookupData(tenants, "k.example", mode); len(data) != 0 {
				t.Errorf("mode %s, %d tenants: the lookup holds %v, want nothing without a custom domain", mode, len(tenants), data)
			}
		}
	}
}
