// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

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
)

func TestReconcileKeycloakIDPGatewayRoutePatchesHTTPRoute(t *testing.T) {
	t.Setenv("SERVICES_NAMESPACE", "gentian-dev")

	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	tenant.Spec.Domain = "demo.desk.gentian.org"

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kernelKeycloakHTTPRouteName(),
			Namespace: "gentian-dev",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: "nubus-dev-keycloak-proxy",
						},
					},
				}},
			}},
		},
	}

	scheme := runtime.NewScheme()
	_ = gatewayv1.Install(scheme)
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant, route).Build()

	if err := reconcileKeycloakIDPGatewayRoute(context.Background(), c, "desk.gentian.org", gentianov1alpha1.TenancyModeMulti); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &gatewayv1.HTTPRoute{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: kernelKeycloakHTTPRouteName(), Namespace: "gentian-dev"}, got); err != nil {
		t.Fatalf("get HTTPRoute: %v", err)
	}
	if len(got.Spec.Rules) != 1 || len(got.Spec.Rules[0].Filters) == 0 {
		t.Fatalf("expected frame-ancestors filters on Keycloak IdP HTTPRoute, got %+v", got.Spec.Rules)
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
	if csp == "" || !strings.Contains(csp, "https://portal.desk.gentian.org") || !strings.Contains(csp, "https://*.demo.desk.gentian.org") {
		t.Fatalf("unexpected CSP: %q", csp)
	}
}
