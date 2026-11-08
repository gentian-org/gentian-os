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
	"sigs.k8s.io/controller-runtime/pkg/client"

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
      oidc:
        clientIdKey: auth.clientId
  secrets:
    generated:
      - name: credential_key
        valuePath: secrets.credentialKey
`

// A chart is told what its profile asked the platform for, over the
// component's own values, which cannot override them.
func TestAChartIsToldWhatItsProfileAskedThePlatformFor(t *testing.T) {
	profile := materialised(t, shopBundle)
	h := startDigestHarness(t, profile, "")
	ctx := context.Background()
	stored := h.reconcile()
	stored.Spec.Config = &gentianov1alpha1.TenantAppConfig{ExtraValues: &runtime.RawExtension{
		Raw: []byte(`{"customerRealm":{"issuer":"https://elsewhere.example"},"web":{"theme":"dark"}}`)}}
	if err := h.c.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	// The release waits for the Secret its values read.
	markSynced(t, h.c, h.comp.Namespace, h.comp.Name+"-sensitive-values")
	h.reconcile()

	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := h.c.Get(ctx, types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
		t.Fatalf("no release: %v", err)
	}
	values, _, _ := unstructured.NestedMap(release.Object, "spec", "forProvider", "values")
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
	before := release.GetResourceVersion()
	h.reconcile()
	if err := h.c.Get(ctx, types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
		t.Fatal(err)
	}
	if release.GetResourceVersion() != before {
		t.Fatal("the release is patched on every pass")
	}
}

// markSynced makes an ExternalSecret report its Secret synced from its
// current generation, as External Secrets does.
func markSynced(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	es := &unstructured.Unstructured{}
	es.SetAPIVersion("external-secrets.io/v1")
	es.SetKind("ExternalSecret")
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, es); err != nil {
		t.Fatalf("no ExternalSecret %s: %v", name, err)
	}
	_ = unstructured.SetNestedSlice(es.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
	}, "status", "conditions")
	if err := c.Update(context.Background(), es); err != nil {
		t.Fatal(err)
	}
}
