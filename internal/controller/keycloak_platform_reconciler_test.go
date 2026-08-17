/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
)

func TestReconcileKeycloakIDPGatewayRoutePatchesHTTPRoute(t *testing.T) {
	// No t.Setenv for SERVICES_NAMESPACE: servicesNamespace is resolved once at
	// package load, so setting it here never reached the code under test. The
	// test passed only while the default happened to equal what it set, and
	// broke the moment the default moved — which is the failure a Setenv that
	// does nothing is designed to hide.

	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	tenant.Spec.Domain = "demo.platform.example.test"

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kernelKeycloakHTTPRouteName(),
			Namespace: "platform-kernel",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: "gentian-idp-keycloak-keycloakx-http",
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

	if err := reconcileKeycloakIDPGatewayRoute(context.Background(), c, "platform.example.test", gentianov1alpha1.TenancyModeMulti); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &gatewayv1.HTTPRoute{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: kernelKeycloakHTTPRouteName(), Namespace: "platform-kernel"}, got); err != nil {
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
	if csp == "" || !strings.Contains(csp, "https://portal.platform.example.test") || !strings.Contains(csp, "https://*.demo.platform.example.test") {
		t.Fatalf("unexpected CSP: %q", csp)
	}
}
