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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// wikiPage is a companion of the wiki profile: a file its bundle brings.
const wikiPage = `apiVersion: v1
kind: ConfigMap
metadata:
  name: wiki.portal-bridge-sso
  labels:
    gentianos.io/profile-name: wiki
    gentianos.io/asset: portal-bridge-sso
data:
  sso.html: "<html></html>"
`

func wikiPageObject() *corev1.ConfigMap {
	page := &corev1.ConfigMap{Data: map[string]string{"sso.html": "<html></html>"}}
	page.Name, page.Namespace = "wiki.portal-bridge-sso", layout.Namespace(layout.Provisioning)
	page.Labels = map[string]string{
		profilebundle.ProfileLabel: "wiki", profilebundle.AssetLabel: "portal-bridge-sso",
		"app.kubernetes.io/instance": "gentian-catalogue-dev",
	}
	return page
}

// An install pinned to a digest is pinned to the whole bundle. While a
// companion the bundle brings is not on the cluster nothing is rolled out,
// and the component looks again by itself -- Argo CD applies a companion
// without the profile or the pin changing. Once it is there the app rolls
// out; changed afterwards, the change is not followed and what runs stays.
func TestAPinnedComponentWaitsForItsBundlesCompanions(t *testing.T) {
	bundle := wikiBundle + "---\n" + wikiPage
	profile := materialised(t, bundle)
	profile.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
	h := startDigestHarness(t, profile, profilebundle.Digest([]byte(bundle)))
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.comp)}

	result, err := h.r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	ready := componentReadyCondition(h.reconcile())
	if ready == nil || ready.Status != "False" || ready.Reason != profilebundle.ReasonCompanionMissing ||
		!strings.Contains(ready.Message, "ConfigMap wiki.portal-bridge-sso") {
		t.Fatalf("condition = %+v, want %s naming the companion", ready, profilebundle.ReasonCompanionMissing)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("a component waiting for a companion does not look again")
	}
	if h.releasedVersion() != "" || h.networkPolicyWritten() {
		t.Fatal("rendered from a bundle that is not all there")
	}

	page := wikiPageObject()
	if err := h.c.Create(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != "Installing" || h.releasedVersion() != "1.0.0" {
		t.Fatalf("with its companion there: condition %+v, release %q", ready, h.releasedVersion())
	}

	page.Data["sso.html"] = "<html>edited in the cluster</html>"
	if err := h.c.Update(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	ready = componentReadyCondition(h.reconcile())
	if ready == nil || ready.Status != "False" || ready.Reason != profilebundle.ReasonCompanionMismatch {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonCompanionMismatch)
	}
	if h.releasedVersion() != "1.0.0" {
		t.Fatal("what was running was taken down")
	}
}

// The same bundle on a profile recorded as a tenant's own is not rolled out,
// companion or no companion: the director refuses it, and the operator does
// not take the director's word that it did.
func TestCompanionsOnATenantsOwnProfileAreNotRolledOut(t *testing.T) {
	bundle := wikiBundle + "---\n" + wikiPage
	profile := materialised(t, bundle)
	profile.Annotations[profilebundle.OriginAnnotation] = profilebundle.TenantOrigin("acme", "own")
	h := startDigestHarness(t, profile, profilebundle.Digest([]byte(bundle)))
	if err := h.c.Create(context.Background(), wikiPageObject()); err != nil {
		t.Fatal(err)
	}
	ready := componentReadyCondition(h.reconcile())
	if ready == nil || ready.Status != "False" || ready.Reason != profilebundle.ReasonRefused {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonRefused)
	}
	if h.releasedVersion() != "" {
		t.Fatal("rolled out")
	}
}

const shopWithComposition = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: shop
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    composition: app-shop
    chart:
      repository: oci://example.invalid/shop
      name: shop
      version: "1.0.0"
---
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: app-shop
  labels:
    gentianos.io/profile-name: shop
spec:
  compositeTypeRef:
    apiVersion: gentianos.io/v1alpha1
    kind: XApp
  mode: Pipeline
  pipeline: []
`

// Which Composition renders an app is the platform's unless the app's own
// came with it: in the bundle the install is pinned to, from a catalogue of
// the cluster. A name in a profile selects nothing by itself.
func TestOnlyAVerifiedBundlesOwnCompositionRendersAnApp(t *testing.T) {
	pinned := &gentianov1alpha1.Component{}
	pinned.Spec.ProfileRef.Name = "shop"
	pinned.Spec.ProfileRef.Digest = profilebundle.Digest([]byte(shopWithComposition))
	unpinned := pinned.DeepCopy()
	unpinned.Spec.ProfileRef.Digest = ""

	own := materialised(t, shopWithComposition)
	own.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
	if got := appComposition(pinned, own); got != "app-shop" {
		t.Fatalf("a pinned install of a bundle bringing its Composition: %q", got)
	}
	if got := appComposition(unpinned, own); got != "" {
		t.Fatalf("an install pinned to nothing: %q", got)
	}

	tenants := own.DeepCopy()
	tenants.Annotations[profilebundle.OriginAnnotation] = profilebundle.TenantOrigin("acme", "own")
	if got := appComposition(pinned, tenants); got != "" {
		t.Fatalf("a Composition on a tenant's own profile: %q", got)
	}

	// A profile that names a Composition and brings none -- another app's,
	// or one put on the cluster by hand -- is rendered by the platform's.
	alone := strings.SplitN(shopWithComposition, "---\n", 2)[0]
	borrowed := materialised(t, strings.Replace(alone, "app-shop", "app-odoo-base-ce", 1))
	borrowed.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
	if got := appComposition(pinned, borrowed); got != "" {
		t.Fatalf("a Composition the bundle does not bring: %q", got)
	}
}

// The claim says which Composition, every time: the profile's own when it
// was given one, the platform's otherwise -- also for a claim that named the
// profile's own before a newer build stopped bringing it.
func TestTheAppClaimNamesItsComposition(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "shop", tenantNamespaceName(tenant), "uid-shop"
	comp.Spec.ProfileRef.Name = "shop"
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", KernelRealm: "kernel"}
	zone := edgeZone{zoneNames: zoneNames{domain: "acme.k.example"}}

	named := func() string {
		t.Helper()
		claim := &unstructured.Unstructured{}
		claim.SetGroupVersionKind(appClaimGVK)
		if err := c.Get(context.Background(), types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, claim); err != nil {
			t.Fatal(err)
		}
		name, _, _ := unstructured.NestedString(claim.Object, "spec", "compositionRef", "name")
		return name
	}
	for _, step := range []struct{ given, want string }{
		{"app-shop", "app-shop"},
		{"", "app-default"},
		{"app-shop", "app-shop"},
	} {
		if _, _, err := r.ensureAppClaim(context.Background(), comp, tenant, zone, pullSecrets{}, step.given, false, nil); err != nil {
			t.Fatal(err)
		}
		if got := named(); got != step.want {
			t.Fatalf("given %q the claim names %q, want %q", step.given, got, step.want)
		}
	}
}
