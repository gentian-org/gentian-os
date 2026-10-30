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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// suppliedProfile is a chart profile with one generated secret and one
// supplied credential of two mapped fields, the second public, and a third
// field the chart is not told.
func suppliedProfile() *gentianov1alpha1.ComponentProfile {
	p := chartProfileFixture("oci://registry.vendor.example/acme/charts")
	p.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{
		Generated: []gentianov1alpha1.AppSecret{{Name: "app_key", ValuePath: "app.key"}, {Name: "unmapped"}},
		Supplied: []gentianov1alpha1.SuppliedSecret{{
			Name: "smtp", DisplayName: "Mail relay", Description: "  The relay's login. ",
			Fields: []gentianov1alpha1.SuppliedField{
				{Key: "password", ValuePath: "mail.password"},
				{Key: "account", ValuePath: "mail.account", Format: "email", Public: true, Example: "ops@example.com"},
				{Key: "note"},
			},
		}},
	}
	return p
}

// The app's own secrets come from where the Composition reads them: a
// generated one as internal-<name> from .../apps/<app>/internal/<name>
// property value, each mapped field of a supplied credential as
// supplied-<credential>-<key> from .../apps/<app>/supplied/<credential>
// property <key>; each told to the chart at its valuePath from the
// component's own Secret. A secret or field with no valuePath asks for
// nothing, as in the Composition (app-default.yaml).
func TestTheAppsOwnSecretsAreReadWhereTheCompositionReadsThem(t *testing.T) {
	comp := sensitiveComponent("wiki")
	got := componentSensitiveValues(comp, suppliedProfile(), "acme")

	want := []struct{ secretKey, path, property, valueKey string }{
		{"internal-app_key", "gentian-os/tenants/acme/apps/wiki/internal/app_key", "value", "app.key"},
		{"supplied-smtp-password", "gentian-os/tenants/acme/apps/wiki/supplied/smtp", "password", "mail.password"},
		{"supplied-smtp-account", "gentian-os/tenants/acme/apps/wiki/supplied/smtp", "account", "mail.account"},
	}
	if len(got.data) != len(want) || len(got.set) != len(want) {
		t.Fatalf("data = %v, set = %v; want %d entries each", got.data, got.set, len(want))
	}
	for i, w := range want {
		d := got.data[i].(map[string]interface{})
		ref := d["remoteRef"].(map[string]interface{})
		if d["secretKey"] != w.secretKey || ref["key"] != w.path || ref["property"] != w.property {
			t.Fatalf("data[%d] = %v, want %s from %s#%s", i, d, w.secretKey, w.path, w.property)
		}
		s := got.set[i].(map[string]interface{})
		from := s["valueFrom"].(map[string]interface{})["secretKeyRef"].(map[string]interface{})
		if s["name"] != w.valueKey || from["name"] != "wiki-sensitive-values" || from["namespace"] != "tenant-acme" || from["key"] != w.secretKey {
			t.Fatalf("set[%d] = %v, want %s from wiki-sensitive-values/%s", i, s, w.valueKey, w.secretKey)
		}
	}
	if composedDelivery(suppliedProfile()) {
		t.Fatal("generated and supplied secrets sent the chart to the Composition")
	}
}

func suppliedHarness(t *testing.T, objects ...client.Object) (*ComponentReconciler, client.Client, *gentianov1alpha1.Component) {
	t.Helper()
	scheme := uninstallScheme(t)
	comp := sensitiveComponent("wiki")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects, comp)...).Build()
	return &ComponentReconciler{Client: c, Scheme: scheme}, c, comp
}

func getProbe(t *testing.T, c client.Client, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	probe := &unstructured.Unstructured{}
	probe.SetGroupVersionKind(externalSecretGVK)
	err := c.Get(context.Background(), types.NamespacedName{Name: "credreq-" + name, Namespace: meta.OperatorNamespace}, probe)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return probe, true
}

func getRequirement(t *testing.T, c client.Client, name string) (*gentianov1alpha1.CredentialRequirement, bool) {
	t.Helper()
	req := &gentianov1alpha1.CredentialRequirement{}
	err := c.Get(context.Background(), types.NamespacedName{Name: name}, req)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return req, true
}

// A supplied credential is declared as the Composition declares it, so the
// requirement the Admin Console lists and the custodian writes to is the
// same one whichever way the app is delivered: named app-<tenant>-<app>-
// <credential>, the same phase, scope, provider and tenant labels, the same
// vault path, display name, description and fields (format string by
// default, secret unless public, the example), validated by noop; and its
// probe credreq-<requirement> in the kernel's namespace, through the
// kernel's store, making no Secret, reading each mapped field. Only the
// managed-by label and the consumer differ: the operator's, and the
// Component rather than the XApp. A second pass writes nothing.
func TestASuppliedCredentialIsDeclaredAsTheCompositionDeclaresIt(t *testing.T) {
	ctx := context.Background()
	r, c, comp := suppliedHarness(t)
	if err := r.ensureSuppliedRequirements(ctx, comp, suppliedProfile(), "acme"); err != nil {
		t.Fatal(err)
	}
	const name = "app-acme-wiki-smtp"
	req, ok := getRequirement(t, c, name)
	if !ok {
		t.Fatal("no requirement declared")
	}
	wantLabels := map[string]string{
		"gentianos.io/credential-phase":    "runtime",
		"gentianos.io/credential-scope":    "tenant",
		"gentianos.io/credential-provider": "wiki",
		"gentianos.io/tenant":              "acme",
		managedByLabel:                     managedByValue,
	}
	if !equalLabels(req.Labels, wantLabels) {
		t.Fatalf("labels = %v, want %v", req.Labels, wantLabels)
	}
	s := req.Spec
	if s.DisplayName != "Mail relay" || s.Description != "The relay's login. The app wiki waits for it until it is set here." ||
		s.Phase != "runtime" || s.Scope != "tenant" || s.Tenant != "acme" ||
		s.VaultPath != "gentian-os/tenants/acme/apps/wiki/supplied/smtp" ||
		s.Validate == nil || s.Validate.Type != "noop" ||
		len(s.ConsumedBy) != 1 || s.ConsumedBy[0].Kind != "Component" || s.ConsumedBy[0].Name != "wiki" {
		t.Fatalf("spec = %+v", s)
	}
	wantFields := []gentianov1alpha1.CredentialField{
		{Key: "password", Format: "string", Secret: true},
		{Key: "account", Format: "email", Secret: false, Example: "ops@example.com"},
	}
	if len(s.Fields) != len(wantFields) {
		t.Fatalf("fields = %+v, want %+v", s.Fields, wantFields)
	}
	for i, w := range wantFields {
		if f := s.Fields[i]; f.Key != w.Key || f.Format != w.Format || f.Secret != w.Secret || f.Example != w.Example {
			t.Fatalf("fields[%d] = %+v, want %+v", i, f, w)
		}
	}

	probe, ok := getProbe(t, c, name)
	if !ok {
		t.Fatal("no probe beside the requirement")
	}
	if l := probe.GetLabels(); l["gentianos.io/credential-requirement"] != name ||
		l["gentianos.io/credential-phase"] != "runtime" || l["gentianos.io/credential-scope"] != "tenant" {
		t.Fatalf("probe labels = %v", l)
	}
	spec, _, _ := unstructured.NestedMap(probe.Object, "spec")
	store, _, _ := unstructured.NestedMap(spec, "secretStoreRef")
	policy, _, _ := unstructured.NestedString(spec, "target", "creationPolicy")
	if spec["refreshInterval"] != "1h" || store["name"] != "openbao" || store["kind"] != "ClusterSecretStore" || policy != "None" {
		t.Fatalf("probe spec = %v", spec)
	}
	data, _, _ := unstructured.NestedSlice(spec, "data")
	if len(data) != 2 {
		t.Fatalf("probe data = %v, want the two mapped fields", data)
	}
	for i, key := range []string{"password", "account"} {
		d := data[i].(map[string]interface{})
		ref := d["remoteRef"].(map[string]interface{})
		if d["secretKey"] != key || ref["key"] != "gentian-os/tenants/acme/apps/wiki/supplied/smtp" || ref["property"] != key {
			t.Fatalf("probe data[%d] = %v", i, d)
		}
	}

	reqVersion, probeVersion := req.ResourceVersion, probe.GetResourceVersion()
	if err := r.ensureSuppliedRequirements(ctx, comp, suppliedProfile(), "acme"); err != nil {
		t.Fatal(err)
	}
	req, _ = getRequirement(t, c, name)
	probe, _ = getProbe(t, c, name)
	if req.ResourceVersion != reqVersion || probe.GetResourceVersion() != probeVersion {
		t.Fatalf("a second pass rewrote what was declared (requirement %s -> %s, probe %s -> %s)",
			reqVersion, req.ResourceVersion, probeVersion, probe.GetResourceVersion())
	}
}

// A description left empty is trimmed as the Composition trims it, and a
// display name left empty is the credential's name; a credential none of
// whose fields the chart is told is declared nowhere.
func TestASuppliedCredentialsDefaultsMatchTheComposition(t *testing.T) {
	ctx := context.Background()
	r, c, comp := suppliedHarness(t)
	p := chartProfileFixture("oci://x")
	p.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Supplied: []gentianov1alpha1.SuppliedSecret{
		{Name: "token", Fields: []gentianov1alpha1.SuppliedField{{Key: "value", ValuePath: "api.token"}}},
		{Name: "untold", Fields: []gentianov1alpha1.SuppliedField{{Key: "value"}}},
	}}
	if err := r.ensureSuppliedRequirements(ctx, comp, p, "acme"); err != nil {
		t.Fatal(err)
	}
	req, ok := getRequirement(t, c, "app-acme-wiki-token")
	if !ok {
		t.Fatal("no requirement declared")
	}
	if req.Spec.DisplayName != "token" || req.Spec.Description != "The app wiki waits for it until it is set here." {
		t.Fatalf("display name %q, description %q", req.Spec.DisplayName, req.Spec.Description)
	}
	if _, ok := getRequirement(t, c, "app-acme-wiki-untold"); ok {
		t.Fatal("a credential the chart is not told of was declared")
	}
}

// A credential dropped from the profile takes its requirement and probe with
// it; one the app Composition declared for the same app -- no managed-by
// label of the operator's -- is the Composition's and survives, and so does
// another app's. On the component's deletion (wanted nil) every one the
// operator declared for it goes, and still not the Composition's.
func TestSuppliedCredentialsNoLongerDeclaredAreRemovedAndTheCompositionsKept(t *testing.T) {
	ctx := context.Background()
	composed := &gentianov1alpha1.CredentialRequirement{ObjectMeta: metav1.ObjectMeta{
		Name: "app-acme-wiki-legacy",
		Labels: map[string]string{
			"gentianos.io/credential-provider": "wiki", "gentianos.io/tenant": "acme",
			"gentianos.io/credential-phase": "runtime", "gentianos.io/credential-scope": "tenant",
		},
	}}
	composedProbe := &unstructured.Unstructured{}
	composedProbe.SetGroupVersionKind(externalSecretGVK)
	composedProbe.SetName("credreq-app-acme-wiki-legacy")
	composedProbe.SetNamespace(meta.OperatorNamespace)
	composedProbe.SetLabels(map[string]string{"gentianos.io/credential-requirement": "app-acme-wiki-legacy"})
	other := &gentianov1alpha1.CredentialRequirement{ObjectMeta: metav1.ObjectMeta{
		Name: "app-acme-shop-smtp",
		Labels: map[string]string{
			"gentianos.io/credential-provider": "shop", "gentianos.io/tenant": "acme", managedByLabel: managedByValue,
		},
	}}
	r, c, comp := suppliedHarness(t, composed, composedProbe, other)

	p := suppliedProfile()
	p.Spec.Secrets.Supplied = append(p.Spec.Secrets.Supplied, gentianov1alpha1.SuppliedSecret{
		Name: "token", Fields: []gentianov1alpha1.SuppliedField{{Key: "value", ValuePath: "api.token"}},
	})
	if err := r.ensureSuppliedRequirements(ctx, comp, p, "acme"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app-acme-wiki-smtp", "app-acme-wiki-token"} {
		if _, ok := getRequirement(t, c, name); !ok {
			t.Fatalf("%s was not declared", name)
		}
	}

	p.Spec.Secrets.Supplied = p.Spec.Secrets.Supplied[:1]
	if err := r.ensureSuppliedRequirements(ctx, comp, p, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, ok := getRequirement(t, c, "app-acme-wiki-token"); ok {
		t.Fatal("a credential dropped from the profile kept its requirement")
	}
	if _, ok := getProbe(t, c, "app-acme-wiki-token"); ok {
		t.Fatal("a credential dropped from the profile kept its probe")
	}
	if _, ok := getRequirement(t, c, "app-acme-wiki-smtp"); !ok {
		t.Fatal("the credential still declared was removed")
	}

	if err := r.dropSuppliedRequirements(ctx, comp, "acme", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := getRequirement(t, c, "app-acme-wiki-smtp"); ok {
		t.Fatal("the component's deletion left its requirement")
	}
	if _, ok := getProbe(t, c, "app-acme-wiki-smtp"); ok {
		t.Fatal("the component's deletion left its probe")
	}
	if _, ok := getRequirement(t, c, "app-acme-wiki-legacy"); !ok {
		t.Fatal("the Composition's requirement was removed")
	}
	if _, ok := getProbe(t, c, "app-acme-wiki-legacy"); !ok {
		t.Fatal("the Composition's probe was removed")
	}
	if _, ok := getRequirement(t, c, "app-acme-shop-smtp"); !ok {
		t.Fatal("another app's requirement was removed")
	}
}

// A directly delivered component with a supplied credential declares it and
// waits with ValuesPending, no Release made, until its sensitive Secret is
// there -- which ESO makes only once a person has set every field; then the
// Release is made with the set entry that reads it.
func TestADirectComponentWaitsForItsSuppliedCredential(t *testing.T) {
	profile := materialised(t, wikiBundle)
	profile.Annotations = nil
	profile.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Supplied: []gentianov1alpha1.SuppliedSecret{{
		Name: "token", Fields: []gentianov1alpha1.SuppliedField{{Key: "value", ValuePath: "api.token"}},
	}}}
	h := startDigestHarness(t, profile, "")

	got := h.reconcile()
	if ready := componentReadyCondition(got); ready == nil || ready.Reason != "ValuesPending" {
		t.Fatalf("condition = %+v, want ValuesPending", ready)
	}
	if h.releasedVersion() != "" {
		t.Fatal("a release was made before the supplied credential was there")
	}
	if _, ok := getRequirement(t, h.c, "app-acme-wiki-token"); !ok {
		t.Fatal("the credential the component waits for was not declared")
	}
	es, ok := getSensitiveValues(t, h.c, h.comp)
	if !ok {
		t.Fatal("no ExternalSecret for the supplied credential")
	}
	markReady(t, h.c, es)

	got = h.reconcile()
	if ready := componentReadyCondition(got); ready == nil || ready.Reason == "ValuesPending" {
		t.Fatalf("condition = %+v, still waiting after the Secret arrived", ready)
	}
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
		t.Fatalf("no release after the Secret arrived: %v", err)
	}
	set, _, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "set")
	if len(set) != 1 || set[0].(map[string]interface{})["name"] != "api.token" {
		t.Fatalf("set = %v", set)
	}
}

// A requirement or probe of the same name that this reconciler did not make
// -- the app Composition's, for an app it delivered -- is never written over:
// two writers would correct each other, and the managed-by label it would
// gain would later have it deleted as this reconciler's own.
func TestASuppliedCredentialTheCompositionDeclaredIsNotTakenOver(t *testing.T) {
	ctx := context.Background()
	scheme := uninstallScheme(t)
	comp := componentFor("fellwort", "tenant-acme")
	profile := chartProfileFixture("oci://registry.vendor.example/acme/charts")
	profile.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Supplied: []gentianov1alpha1.SuppliedSecret{{
		Name: "license", DisplayName: "License", Fields: []gentianov1alpha1.SuppliedField{{Key: "key", ValuePath: "license.key"}},
	}}}
	name := suppliedRequirementName("acme", "fellwort", "license")
	composed := &gentianov1alpha1.CredentialRequirement{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{suppliedProviderLabel: "fellwort"}}}
	composed.Spec.DisplayName = "composed"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(composed).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	if err := r.ensureSuppliedRequirements(ctx, comp, profile, "acme"); err == nil {
		t.Fatal("a requirement the Composition declared was taken over")
	}
	got := &gentianov1alpha1.CredentialRequirement{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.DisplayName != "composed" || got.Labels[managedByLabel] != "" {
		t.Fatalf("the Composition's requirement was written over: %+v", got)
	}
}
