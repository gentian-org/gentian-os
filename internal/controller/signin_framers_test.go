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
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func signInTenant(name string, from []string, to ...string) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, f := range from {
		t.Spec.SignInFrom = append(t.Spec.SignInFrom, gentianov1alpha1.TenantSignInFrom{Tenant: f})
	}
	t.Spec.SignInTo = to
	return t
}

// The desktop of a tenant whose people sign in to another frames that
// tenant's apps, where both declare the sign-in, and no other desktop does.
func TestTheDesktopsOfSignInSourcesFrameATenantsApps(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	// customers takes team and staff, and the platform tenant; team names
	// customers back, staff does not, the platform tenant does.
	customers := signInTenant("customers", []string{"team", "staff", gentianov1alpha1.PlatformTenantName, "gone", "customers"})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		customers,
		signInTenant("team", nil, "customers"),
		signInTenant("staff", nil),
		signInTenant(gentianov1alpha1.PlatformTenantName, nil, "customers"),
		signInTenant("lonely", nil, "customers"),
	).Build()
	r := &ComponentReconciler{Client: c, KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: gentianov1alpha1.TenancyModeMulti}

	got, err := r.signInDesktops(ctx, customers)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"desktop.team.k.example"}; !slices.Equal(got, want) {
		t.Fatalf("framing desktops %v, want %v", got, want)
	}

	// lonely names customers, but customers does not name lonely.
	lonely := signInTenant("lonely", nil, "customers")
	if got, err := r.signInDesktops(ctx, lonely); err != nil || len(got) != 0 {
		t.Fatalf("a tenant nobody signs in to is framed by %v (%v)", got, err)
	}

	zone := r.zoneOf(customers)
	framers := componentFramers(zone, &gentianov1alpha1.Component{}, &gentianov1alpha1.ComponentProfile{}, "cms.customers.k.example", got)
	if !slices.Equal(framers, []string{"desktop.customers.k.example", "desktop.team.k.example"}) {
		t.Fatalf("framers %v", framers)
	}
}
