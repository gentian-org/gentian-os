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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// shopBundle is a chart the app Composition delivers -- it declares a secret
// the Composition generates -- and asks the platform for its realm's issuer.
const shopBundle = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: shop
  labels:
    gentianos.io/profile-name: shop
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    chart:
      repository: oci://example.invalid/shop
      name: shop
      version: "1.0.0"
    valueMapping:
      platform:
        issuerKey: customerRealm.issuer
        tenantKey: tenant
  secrets:
    generated:
      - name: credential_key
        valuePath: secrets.credentialKey
`

// A chart the Composition delivers is told what its profile asked the
// platform for, just as one the operator installs itself: the values travel
// on the claim the Composition renders the release from, over the
// component's own, which cannot override them. Before, they were computed and
// dropped, and a chart that needs its issuer could not start.
func TestAComposedChartIsToldWhatItsProfileAskedThePlatformFor(t *testing.T) {
	profile := materialised(t, shopBundle)
	if !composedDelivery(profile) {
		t.Fatal("the fixture is meant to be an app the Composition installs")
	}
	h := startDigestHarness(t, profile, "")
	ctx := context.Background()
	stored := h.reconcile()
	stored.Spec.Config = &gentianov1alpha1.TenantAppConfig{ExtraValues: &runtime.RawExtension{
		Raw: []byte(`{"customerRealm":{"issuer":"https://elsewhere.example"},"web":{"theme":"dark"}}`)}}
	if err := h.c.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	h.reconcile()

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(appClaimGVK)
	if err := h.c.Get(ctx, types.NamespacedName{Name: h.comp.Name, Namespace: h.comp.Namespace}, claim); err != nil {
		t.Fatalf("no claim: %v", err)
	}
	values, _, _ := unstructured.NestedMap(claim.Object, "spec", "config", "extraValues")
	issuer, _, _ := unstructured.NestedString(values, "customerRealm", "issuer")
	if issuer != "https://id.k.example/auth/realms/acme" {
		t.Fatalf("customerRealm.issuer = %q, want the tenant realm's issuer (values %v)", issuer, values)
	}
	if tenant, _, _ := unstructured.NestedString(values, "tenant"); tenant != "acme" {
		t.Fatalf("tenant = %q", tenant)
	}
	if theme, _, _ := unstructured.NestedString(values, "web", "theme"); theme != "dark" {
		t.Fatalf("the component's own values were lost: %v", values)
	}

	// A second pass finds nothing to change.
	before := claim.GetResourceVersion()
	h.reconcile()
	if err := h.c.Get(ctx, types.NamespacedName{Name: h.comp.Name, Namespace: h.comp.Namespace}, claim); err != nil {
		t.Fatal(err)
	}
	if claim.GetResourceVersion() != before {
		t.Fatal("the claim is patched on every pass")
	}
}
