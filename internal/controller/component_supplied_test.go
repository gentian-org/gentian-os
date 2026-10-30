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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// suppliedRender renders the wiki of tenant acme from a profile, with the
// kernel's namespace the credential probes are placed in.
func suppliedRender(t *testing.T, profile *gentianov1alpha1.ComponentProfile) []*unstructured.Unstructured {
	t.Helper()
	in := chartRenderInput("wiki", profile)
	in.ClusterConfig = map[string]string{"kernel.controlNamespace": meta.OperatorNamespace}
	return renderApp(t, in)
}

// The app's own secrets come from where the Composition read them: a
// generated one as internal-<name> from .../apps/<app>/internal/<name>
// property value, each mapped field of a supplied credential as
// supplied-<credential>-<key> from .../apps/<app>/supplied/<credential>
// property <key>; each told to the chart at its valuePath from the
// component's own Secret. A secret or field with no valuePath asks for
// nothing.
func TestTheAppsOwnSecretsAreReadWhereTheCompositionReadsThem(t *testing.T) {
	data, set := sensitiveOf(t, suppliedRender(t, suppliedProfile()), "wiki")

	want := []struct{ secretKey, path, property, valueKey string }{
		{"internal-app_key", "gentian-os/tenants/acme/apps/wiki/internal/app_key", "value", "app.key"},
		{"supplied-smtp-password", "gentian-os/tenants/acme/apps/wiki/supplied/smtp", "password", "mail.password"},
		{"supplied-smtp-account", "gentian-os/tenants/acme/apps/wiki/supplied/smtp", "account", "mail.account"},
	}
	if len(data) != len(want) || len(set) != len(want) {
		t.Fatalf("data = %v, set = %v; want %d entries each", data, set, len(want))
	}
	for i, w := range want {
		d := data[i]
		ref := d["remoteRef"].(map[string]interface{})
		if d["secretKey"] != w.secretKey || ref["key"] != w.path || ref["property"] != w.property {
			t.Fatalf("data[%d] = %v, want %s from %s#%s", i, d, w.secretKey, w.path, w.property)
		}
		s := set[i]
		from := s["valueFrom"].(map[string]interface{})["secretKeyRef"].(map[string]interface{})
		if s["name"] != w.valueKey || from["name"] != "wiki-sensitive-values" || from["namespace"] != "tenant-acme" || from["key"] != w.secretKey {
			t.Fatalf("set[%d] = %v, want %s from wiki-sensitive-values/%s", i, s, w.valueKey, w.secretKey)
		}
	}
}

// suppliedHarness is the wiki of tenant acme with a profile that declares
// the given supplied credentials, each of one mapped field.
func suppliedHarness(t *testing.T, credentials ...string) *digestHarness {
	t.Helper()
	profile := materialised(t, wikiBundle)
	profile.Annotations = nil
	profile.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{}
	for _, name := range credentials {
		profile.Spec.Secrets.Supplied = append(profile.Spec.Secrets.Supplied, gentianov1alpha1.SuppliedSecret{
			Name: name, Fields: []gentianov1alpha1.SuppliedField{{Key: "value", ValuePath: "api." + name}},
		})
	}
	return startDigestHarness(t, profile, "")
}

// A supplied credential is declared as the Composition declared it, so the
// requirement the Admin Console lists and the custodian writes to is the
// same one whoever installed the app: named app-<tenant>-<app>-<credential>,
// the same phase, scope, provider and tenant labels, the same vault path,
// display name, description and fields (format string by default, secret
// unless public, the example), validated by noop; and its probe
// credreq-<requirement> in the kernel's namespace, through the kernel's
// store, making no Secret, reading each mapped field. Only the consumer
// differs: the Component rather than the XApp. Applied, a second pass writes
// nothing.
func TestASuppliedCredentialIsDeclaredAsTheCompositionDeclaresIt(t *testing.T) {
	const name = "app-acme-wiki-smtp"
	objs := suppliedRender(t, suppliedProfile())
	rendered := renderedNamed(objs, "CredentialRequirement", name)
	if rendered == nil {
		t.Fatal("no requirement declared")
	}
	req := &gentianov1alpha1.CredentialRequirement{}
	convertYAML(t, rendered.Object, req)
	for k, v := range map[string]string{
		"gentianos.io/credential-phase":    "runtime",
		"gentianos.io/credential-scope":    "tenant",
		"gentianos.io/credential-provider": "wiki",
		"gentianos.io/tenant":              "acme",
	} {
		if req.Labels[k] != v {
			t.Fatalf("labels = %v, want %s=%s", req.Labels, k, v)
		}
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

	probe := renderedNamed(objs, "ExternalSecret", "credreq-"+name)
	if probe == nil || probe.GetNamespace() != meta.OperatorNamespace {
		t.Fatalf("no probe beside the requirement in %s: %v", meta.OperatorNamespace, probe)
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

	// Applied: both carry the component's labels, and a second pass
	// rewrites neither.
	h := suppliedHarness(t, "smtp")
	h.reconcile()
	applied, ok := getRequirement(t, h.c, name)
	if !ok {
		t.Fatal("the requirement was not applied")
	}
	appliedProbe, ok := getProbe(t, h.c, name)
	if !ok {
		t.Fatal("the probe was not applied")
	}
	if applied.Labels[componentLabel] != "wiki" || appliedProbe.GetLabels()[componentLabel] != "wiki" {
		t.Fatalf("labels: requirement %v, probe %v", applied.Labels, appliedProbe.GetLabels())
	}
	h.reconcile()
	again, _ := getRequirement(t, h.c, name)
	againProbe, _ := getProbe(t, h.c, name)
	if again.ResourceVersion != applied.ResourceVersion || againProbe.GetResourceVersion() != appliedProbe.GetResourceVersion() {
		t.Fatalf("a second pass rewrote what was declared (requirement %s -> %s, probe %s -> %s)",
			applied.ResourceVersion, again.ResourceVersion, appliedProbe.GetResourceVersion(), againProbe.GetResourceVersion())
	}
}

// A description left empty is trimmed as the Composition trimmed it, and a
// display name left empty is the credential's name.
func TestASuppliedCredentialsDefaultsMatchTheComposition(t *testing.T) {
	p := chartProfileFixture("oci://x")
	p.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Supplied: []gentianov1alpha1.SuppliedSecret{
		{Name: "token", Fields: []gentianov1alpha1.SuppliedField{{Key: "value", ValuePath: "api.token"}}},
	}}
	objs := suppliedRender(t, p)
	rendered := renderedNamed(objs, "CredentialRequirement", "app-acme-wiki-token")
	if rendered == nil {
		t.Fatal("no requirement declared")
	}
	req := &gentianov1alpha1.CredentialRequirement{}
	convertYAML(t, rendered.Object, req)
	if req.Spec.DisplayName != "token" || req.Spec.Description != "The app wiki waits for it until it is set here." {
		t.Fatalf("display name %q, description %q", req.Spec.DisplayName, req.Spec.Description)
	}
}

// A credential dropped from the profile takes its requirement and probe with
// it, found again through the component's inventory: both are outside the
// component's namespace, where no owner reference reaches. One the app
// Composition declared for the same app and nobody took over is not the
// component's and survives, and so does another app's. On the component's
// deletion every one it declared goes, still not the others, and the
// component is not gone before its release is.
func TestSuppliedCredentialsNoLongerDeclaredAreRemovedAndOthersKept(t *testing.T) {
	ctx := context.Background()
	h := suppliedHarness(t, "smtp", "token")
	composed := &gentianov1alpha1.CredentialRequirement{ObjectMeta: metav1.ObjectMeta{
		Name: "app-acme-wiki-legacy",
		Labels: map[string]string{
			"gentianos.io/credential-provider": "wiki", "gentianos.io/tenant": "acme",
			"gentianos.io/credential-phase": "runtime", "gentianos.io/credential-scope": "tenant",
		},
	}}
	other := &gentianov1alpha1.CredentialRequirement{ObjectMeta: metav1.ObjectMeta{
		Name:   "app-acme-shop-smtp",
		Labels: map[string]string{"gentianos.io/credential-provider": "shop", "gentianos.io/tenant": "acme", componentLabel: "shop"},
	}}
	for _, obj := range []client.Object{composed, other} {
		if err := h.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	h.reconcileInstalled()
	for _, name := range []string{"app-acme-wiki-smtp", "app-acme-wiki-token"} {
		if _, ok := getRequirement(t, h.c, name); !ok {
			t.Fatalf("%s was not declared", name)
		}
		if _, ok := getProbe(t, h.c, name); !ok {
			t.Fatalf("%s has no probe", name)
		}
	}

	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) { p.Spec.Secrets.Supplied = p.Spec.Secrets.Supplied[:1] })
	h.reconcileInstalled()
	if _, ok := getRequirement(t, h.c, "app-acme-wiki-token"); ok {
		t.Fatal("a credential dropped from the profile kept its requirement")
	}
	if _, ok := getProbe(t, h.c, "app-acme-wiki-token"); ok {
		t.Fatal("a credential dropped from the profile kept its probe")
	}
	if _, ok := getRequirement(t, h.c, "app-acme-wiki-smtp"); !ok {
		t.Fatal("the credential still declared was removed")
	}
	for _, key := range inventoryOf(getComponent(t, h.c, h.comp)) {
		if strings.Contains(key, "app-acme-wiki-token") {
			t.Fatalf("the inventory still lists %s", key)
		}
	}

	// The component is deleted. Its release is held by provider-helm while
	// it uninstalls the chart.
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	if err := h.c.Get(ctx, types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil {
		t.Fatalf("no release: %v", err)
	}
	release.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
	if err := h.c.Update(ctx, release); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(ctx, getComponent(t, h.c, h.comp)); err != nil {
		t.Fatal(err)
	}
	res, err := h.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.comp)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := getRequirement(t, h.c, "app-acme-wiki-smtp"); ok {
		t.Fatal("the component's deletion left its requirement")
	}
	if _, ok := getProbe(t, h.c, "app-acme-wiki-smtp"); ok {
		t.Fatal("the component's deletion left its probe")
	}
	if err := h.c.Get(ctx, types.NamespacedName{Name: releaseName(h.comp)}, release); err != nil || release.GetDeletionTimestamp().IsZero() {
		t.Fatalf("the release is not being uninstalled: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("the component let go while its release was still being uninstalled")
	}
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(h.comp), &gentianov1alpha1.Component{}); err != nil {
		t.Fatalf("the component is gone before its release is: %v", err)
	}

	// provider-helm finishes; the component goes.
	release.SetFinalizers(nil)
	if err := h.c.Update(ctx, release); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.comp)}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(h.comp), &gentianov1alpha1.Component{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the component stayed after its release was gone: %v", err)
	}
	if _, ok := getRequirement(t, h.c, "app-acme-wiki-legacy"); !ok {
		t.Fatal("the Composition's requirement was removed")
	}
	if _, ok := getRequirement(t, h.c, "app-acme-shop-smtp"); !ok {
		t.Fatal("another app's requirement was removed")
	}
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

// A component with a supplied credential declares it and waits with
// ValuesPending, no Release made, until its sensitive Secret is there --
// which ESO makes only once a person has set every field; then the Release
// is made with the set entry that reads it.
func TestAComponentWaitsForItsSuppliedCredential(t *testing.T) {
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

// A requirement of the same name that the component did not make -- the app
// Composition's, for an app nobody took over -- is never written over: two
// writers would correct each other, and the labels it would gain would later
// have it deleted as the component's own.
func TestASuppliedCredentialTheCompositionDeclaredIsNotTakenOver(t *testing.T) {
	ctx := context.Background()
	h := suppliedHarness(t, "license")
	name := "app-acme-wiki-license"
	composed := &gentianov1alpha1.CredentialRequirement{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"gentianos.io/credential-provider": "wiki"}}}
	composed.Spec.DisplayName = "composed"
	if err := h.c.Create(ctx, composed); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.comp)}); err == nil {
		t.Fatal("a requirement the Composition declared was taken over")
	}
	got, _ := getRequirement(t, h.c, name)
	if got.Spec.DisplayName != "composed" || got.Labels[componentLabel] != "" {
		t.Fatalf("the Composition's requirement was written over: %+v", got)
	}
}
