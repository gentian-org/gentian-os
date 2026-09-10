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
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKernelPortalHost(t *testing.T) {
	t.Parallel()
	if got := kernelPortalHost("platform.example.test"); got != "portal.platform.example.test" {
		t.Fatalf("kernelPortalHost = %q", got)
	}
	if kernelPortalHost("") != "" {
		t.Fatal("expected empty portal host for empty kernel domain")
	}
}

// The tenant host serves the portal directly now, so there is no redirect to
// assert. What must hold is that the old one is removed: two routes claiming one
// hostname is undefined, so it has to be deleted rather than merely left
// unreconciled.

func TestPortalRedirectIsDeletedNotRecreated(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	existing := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantApexRedirectRouteName("demo"),
			Namespace: "tenant-demo",
		},
	}
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme, KernelDomain: "platform.example.test"}

	if err := r.ensurePortalRedirect(context.Background(), tenant); err != nil {
		t.Fatalf("ensurePortalRedirect: %v", err)
	}
	var got gatewayv1.HTTPRoute
	err := c.Get(context.Background(),
		client.ObjectKey{Name: tenantApexRedirectRouteName("demo"), Namespace: "tenant-demo"}, &got)
	if !errors.IsNotFound(err) {
		t.Fatalf("legacy redirect still present (err=%v) — it would compete with the portal route", err)
	}

	// Idempotent: nothing to delete on the next pass.
	if err := r.ensurePortalRedirect(context.Background(), tenant); err != nil {
		t.Fatalf("second pass: %v", err)
	}
}

func TestTenantHostGetsAPortalRouteWithTheSameBackends(t *testing.T) {
	t.Parallel()
	specs := kernelHTTPRouteSpecs(
		"platform.example.test",
		[]string{"demo.platform.example.test"},
		nil,
		[]string{"demo"},
		false,
	)
	var found *kernelHTTPRouteSpec
	for i := range specs {
		if specs[i].name == "tenant-demo-portal" {
			found = &specs[i]
		}
	}
	if found == nil {
		t.Fatal("no portal route for the tenant host")
	}
	if found.host != "demo.platform.example.test" {
		t.Fatalf("host = %q", found.host)
	}
	// Same backends as the shared route: one portal deployment answering on more
	// names, not a copy per tenant.
	if len(found.rules) != len(kernelGentianPortalHTTPRouteRules()) {
		t.Fatalf("expected the shared portal rules, got %d", len(found.rules))
	}
	// The API has to answer on the tenant host too, or the SPA cannot call it.
	var hasAPI bool
	for _, rule := range found.rules {
		for _, b := range rule.BackendRefs {
			if string(b.Name) == gentianPortalAPIService {
				hasAPI = true
			}
		}
	}
	if !hasAPI {
		t.Fatal("tenant host must route /api to the portal API")
	}
}
