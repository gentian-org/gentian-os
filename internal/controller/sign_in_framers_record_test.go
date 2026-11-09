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
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// signInFramerProfile is an app profile whose gateway entries sign in with
// OIDC, one entry per sub-domain.
func signInFramerProfile(name string, subDomains ...string) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone,
			TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{ClientID: name + "-oidc"}},
			}},
		},
	}
	for _, sub := range subDomains {
		p.Spec.Expose = append(p.Spec.Expose, gentianov1alpha1.ExposureSpec{
			Name: sub, Surface: gentianov1alpha1.SurfaceGateway, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: sub,
		})
	}
	return p
}

// signInFramerTenant is a tenant with the named apps installed, each as a
// Component of the same name in the tenant's namespace.
func signInFramerTenant(name, domain, realm string, apps ...string) []client.Object {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	tenant.Status.Domain = domain
	if realm != "" {
		tenant.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: realm}
	}
	objs := []client.Object{tenant}
	for _, app := range apps {
		tenant.Spec.Apps = append(tenant.Spec.Apps, gentianov1alpha1.TenantApp{Profile: app})
		objs = append(objs, &gentianov1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{Name: app, Namespace: tenant.NamespaceName()},
			Spec:       gentianov1alpha1.ComponentSpec{ProfileRef: gentianov1alpha1.ProfileRef{Name: app}},
		})
	}
	return objs
}

// TestRecordSignInFramers records which pages may frame the identity
// provider's sign-in pages: the frame-ancestors sources its route carries
// after the reconciler has run, for a multi-tenancy and a single-tenancy
// cluster with one app on one host and one app on two.
func TestRecordSignInFramers(t *testing.T) {
	const kernel = "example.test"
	profiles := []client.Object{
		signInFramerProfile("wiki", "wiki"),
		signInFramerProfile("files", "files", "office"),
	}
	cases := []struct {
		name, mode string
		objs       []client.Object
	}{
		{"multi", gentianov1alpha1.TenancyModeMulti, concatObjects(
			signInFramerTenant(gentianov1alpha1.PlatformTenantName, "", "kernel"),
			signInFramerTenant("demo", "", "", "wiki", "files"),
			signInFramerTenant("acme", "acme.ch", "", "wiki"),
		)},
		{"single", gentianov1alpha1.TenancyModeSingle, concatObjects(
			signInFramerTenant(gentianov1alpha1.PlatformTenantName, "", "kernel"),
			signInFramerTenant(gentianov1alpha1.SingleUserTenantName, "", "", "wiki", "files"),
		)},
	}
	var b strings.Builder
	for _, tc := range cases {
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: kernelKeycloakHTTPRouteName(), Namespace: layout.Namespace(layout.Edge)},
			Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS("gentian-idp-keycloak-keycloakx-http", "", 8080, "/auth/realms/"),
			}},
		}
		scheme := runtime.NewScheme()
		_ = gatewayv1.Install(scheme)
		_ = gentianov1alpha1.AddToScheme(scheme)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(append([]client.Object{route}, profiles...), tc.objs...)...).Build()
		ctx := context.Background()
		if err := recordSignInFramersReconcile(ctx, c, kernel, tc.mode); err != nil {
			t.Fatalf("%s: reconcile: %v", tc.name, err)
		}
		got := &gatewayv1.HTTPRoute{}
		if err := c.Get(ctx, types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, got); err != nil {
			t.Fatal(err)
		}
		sources, _ := frameAncestors(contentSecurityPolicy(got.Spec.Rules[0].Filters))
		fmt.Fprintf(&b, "%s:\n", tc.name)
		for _, s := range sources {
			fmt.Fprintf(&b, "  %s\n", s)
		}
	}
	holdToTranscript(t, *updateTranscripts, "sign-in-framers", b.String())
}

func concatObjects(lists ...[]client.Object) []client.Object {
	var out []client.Object
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// recordSignInFramersReconcile runs the identity provider's route reconciler
// as the operator does.
func recordSignInFramersReconcile(ctx context.Context, c client.Client, kernel, mode string) error {
	return reconcileKeycloakIDPGatewayRoute(ctx, c, kernel, mode)
}
