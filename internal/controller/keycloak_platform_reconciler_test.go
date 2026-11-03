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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func TestReconcileKeycloakIDPGatewayRoutePatchesHTTPRoute(t *testing.T) {
	// No t.Setenv for SERVICES_NAMESPACE: servicesNamespace is resolved once at
	// package load, so setting it here never reached the code under test. The
	// test passed only while the default happened to equal what it set, and
	// broke the moment the default moved — which is the failure a Setenv that
	// does nothing is designed to hide.

	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	tenant.Status.Domain = "demo.platform.example.test"

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kernelKeycloakHTTPRouteName(),
			Namespace: layout.Namespace(layout.Edge),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			// Two rules, as the kernel route builder writes them: one per
			// allowed prefix on the identity provider's perimeter.
			Rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS("gentian-idp-keycloak-keycloakx-http", "", 8080, "/auth/realms/"),
				kernelBackendRulePrefixNS("gentian-idp-keycloak-keycloakx-http", "", 8080, "/auth/resources/"),
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = gatewayv1.Install(scheme)
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, route).Build()

	if err := reconcileKeycloakIDPGatewayRoute(context.Background(), c, "platform.example.test", gentianov1alpha1.TenancyModeMulti); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &gatewayv1.HTTPRoute{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: kernelKeycloakHTTPRouteName(), Namespace: layout.Namespace(layout.Edge)}, got); err != nil {
		t.Fatalf("get HTTPRoute: %v", err)
	}
	// Every rule keeps its place and gets the filters. Rebuilding the route
	// from rule zero alone collapsed it to one prefix and left id.<kernel>
	// answering 404 on the other.
	if len(got.Spec.Rules) != 2 {
		t.Fatalf("expected both rules to survive the frame-ancestors patch, got %+v", got.Spec.Rules)
	}
	for i := range got.Spec.Rules {
		if len(got.Spec.Rules[i].Filters) == 0 {
			t.Fatalf("rule %d has no frame-ancestors filters: %+v", i, got.Spec.Rules[i])
		}
	}
	modifier := got.Spec.Rules[0].Filters[0].ResponseHeaderModifier
	if modifier == nil {
		t.Fatal("expected ResponseHeaderModifier filter")
	}
	var csp string
	for _, h := range modifier.Set {
		if h.Name == "Content-Security-Policy" {
			csp = h.Value
			break
		}
	}
	if csp == "" || !strings.Contains(csp, "https://platform.platform.example.test") || !strings.Contains(csp, "https://*.demo.platform.example.test") {
		t.Fatalf("unexpected CSP: %q", csp)
	}
}

// The policy counts as applied only when every rule's frame-ancestors
// directive allows exactly the sources wanted: a header that names them all
// and one more lets a page nobody allowed frame the realm.
func TestTheFramePolicyIsAppliedOnlyWhenItsSourcesAreExact(t *testing.T) {
	ctx := context.Background()
	const kernel = "platform.example.test"
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	tenant.Status.Domain = "demo.platform.example.test"
	want := contentSecurityPolicy(keycloakGatewayResponseFilters(kernel, []string{tenant.Status.Domain}, nil, []string{tenant.Name}))

	withPolicy := func(policies ...string) *gatewayv1.HTTPRoute {
		route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
			Name: kernelKeycloakHTTPRouteName(), Namespace: layout.Namespace(layout.Edge),
		}}
		for _, policy := range policies {
			rule := kernelBackendRulePrefixNS("gentian-idp-keycloak-keycloakx-http", "", 8080, "/auth/realms/")
			if policy != "" {
				rule.Filters = []gatewayv1.HTTPRouteFilter{{
					Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
					ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
						Set: []gatewayv1.HTTPHeader{{Name: "Content-Security-Policy", Value: policy}},
					},
				}}
			}
			route.Spec.Rules = append(route.Spec.Rules, rule)
		}
		return route
	}
	sources, _ := frameAncestors(want)
	reversed := make([]string, len(sources))
	for i, s := range sources {
		reversed[len(sources)-1-i] = s
	}
	for name, tc := range map[string]struct {
		route *gatewayv1.HTTPRoute
		want  bool
	}{
		"as written":               {withPolicy(want, want), true},
		"in another order":         {withPolicy("default-src 'self'; frame-ancestors " + strings.Join(reversed, " ")), true},
		"one source more":          {withPolicy(want + " https://elsewhere.example"), false},
		"one source fewer":         {withPolicy("frame-ancestors " + strings.Join(sources[1:], " ")), false},
		"a wider wildcard":         {withPolicy(strings.Replace(want, "https://*.demo.platform.example.test", "https://*", 1)), false},
		"one rule without it":      {withPolicy(want, ""), false},
		"no frame-ancestors":       {withPolicy("default-src 'self' " + strings.Join(sources, " ")), false},
		"inside another directive": {withPolicy("sandbox " + want), false},
	} {
		scheme := runtime.NewScheme()
		_ = gatewayv1.Install(scheme)
		_ = gentianov1alpha1.AddToScheme(scheme)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), tc.route).Build()
		if got := keycloakGatewayFramePolicyApplied(ctx, c, kernel, gentianov1alpha1.TenancyModeMulti); got != tc.want {
			t.Errorf("%s: applied = %v, want %v", name, got, tc.want)
		}
	}
}
