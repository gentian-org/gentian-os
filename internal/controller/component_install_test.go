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
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

func componentNames(t *testing.T, c client.Client, namespace string) string {
	t.Helper()
	list := &gentianov1alpha1.ComponentList{}
	if err := c.List(context.Background(), list, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, comp := range list.Items {
		names = append(names, comp.Name)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// Every app a tenant installs is a Component, and so is every addon activated
// inside one. Removing the entry removes the Component -- and only a
// Component that was an install, never one the platform ships to everybody.
func TestEveryInstalledAppIsAComponent(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	replicas := int32(2)
	pinned := "sha256:" + strings.Repeat("ab", 32)
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{
		{Profile: "xwiki-ce", Digest: pinned, Config: &gentianov1alpha1.TenantAppConfig{Replicas: &replicas}},
		{Profile: "odoo-base-ce", Addons: []string{"crm-ce", "sales-ce"},
			AddonPins: []gentianov1alpha1.AddonPin{
				{Name: "sales-ce", Digest: pinned, Catalogue: "main"},
				// A pin for an addon that is not activated pins nothing.
				{Name: "website-ce", Digest: pinned},
			}},
	}
	ns := tenantNamespaceName(tenant)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		profileFixture("desktop", true, gentianov1alpha1.ComponentClassApp),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	if err := r.ensureDefaultComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if got := componentNames(t, c, ns); got != "crm-ce desktop odoo-base-ce sales-ce xwiki-ce" {
		t.Fatalf("components = %q", got)
	}
	base := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, types.NamespacedName{Name: "odoo-base-ce", Namespace: ns}, base); err != nil {
		t.Fatal(err)
	}
	// The base carries the list of what is activated inside it, because the
	// base's chart is what activates it.
	if strings.Join(base.Spec.Addons, " ") != "crm-ce sales-ce" || base.Spec.Class != gentianov1alpha1.ComponentClassApp ||
		base.Labels[componentOriginLabel] != componentOriginInstall {
		t.Fatalf("base = %+v labels %v", base.Spec, base.Labels)
	}
	// An addon pinned to a build carries the pin on its own Component, and
	// the base carries it beside the list: the base's release is where the
	// addon takes effect, so the base is what is held for it. An addon with
	// no pin has none in either place.
	if len(base.Spec.AddonPins) != 1 || base.Spec.AddonPins[0].Name != "sales-ce" || base.Spec.AddonPins[0].Digest != pinned {
		t.Fatalf("the base's addon pins = %+v", base.Spec.AddonPins)
	}
	for addon, want := range map[string]string{"sales-ce": pinned, "crm-ce": ""} {
		comp := &gentianov1alpha1.Component{}
		if err := c.Get(ctx, types.NamespacedName{Name: addon, Namespace: ns}, comp); err != nil {
			t.Fatal(err)
		}
		if comp.Spec.ProfileRef.Digest != want {
			t.Fatalf("addon %s is pinned to %q, want %q", addon, comp.Spec.ProfileRef.Digest, want)
		}
	}
	wiki := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, types.NamespacedName{Name: "xwiki-ce", Namespace: ns}, wiki); err != nil {
		t.Fatal(err)
	}
	if wiki.Spec.Config == nil || wiki.Spec.Config.Replicas == nil || *wiki.Spec.Config.Replicas != 2 {
		t.Fatalf("the install's configuration did not reach its component: %+v", wiki.Spec.Config)
	}
	// The build the install is pinned to is the Component's, under the same
	// name: the digest is a field, and an app installed at no digest has none.
	if wiki.Spec.ProfileRef.Name != "xwiki-ce" || wiki.Spec.ProfileRef.Digest != pinned {
		t.Fatalf("the install's digest did not reach its component: %+v", wiki.Spec.ProfileRef)
	}
	if base.Spec.ProfileRef.Digest != "" {
		t.Fatalf("an install with no digest is pinned to %q", base.Spec.ProfileRef.Digest)
	}
	// The pin moves when the Tenant's does.
	moved := "sha256:" + strings.Repeat("cd", 32)
	tenant.Spec.Apps[0].Digest = moved
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "xwiki-ce", Namespace: ns}, wiki); err != nil {
		t.Fatal(err)
	}
	if wiki.Spec.ProfileRef.Digest != moved {
		t.Fatalf("the component is still pinned to %q", wiki.Spec.ProfileRef.Digest)
	}

	// An addon is deactivated and an app uninstalled: both go, the base
	// learns its new list, and the platform's own component is untouched.
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "odoo-base-ce", Addons: []string{"crm-ce"}}}
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if got := componentNames(t, c, ns); got != "crm-ce desktop odoo-base-ce" {
		t.Fatalf("after uninstall, components = %q", got)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "odoo-base-ce", Namespace: ns}, base); err != nil {
		t.Fatal(err)
	}
	if strings.Join(base.Spec.Addons, " ") != "crm-ce" {
		t.Fatalf("the base still lists %v", base.Spec.Addons)
	}
	if len(base.Spec.AddonPins) != 0 {
		t.Fatalf("the base still carries the pin of an addon that is gone: %+v", base.Spec.AddonPins)
	}

	// Nothing installed at all.
	tenant.Spec.Apps = nil
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if got := componentNames(t, c, ns); got != "desktop" {
		t.Fatalf("with nothing installed, components = %q", got)
	}
}

// A component delivered through the app Composition writes the claim that
// Composition answers, owns it, and is not ready before the claim is.
func TestAComposedComponentWritesItsClaim(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	replicas := int32(3)
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "odoo-base-ce", tenantNamespaceName(tenant), "uid-comp"
	comp.Spec.ProfileRef.Name = "odoo-base-ce"
	comp.Spec.Addons = []string{"crm-ce"}
	comp.Spec.Config = &gentianov1alpha1.TenantAppConfig{
		Replicas:    &replicas,
		ExtraValues: &runtime.RawExtension{Raw: []byte(`{"web":{"theme":"dark"}}`)},
	}

	// A claim nobody controls: adopted where it stands, because replacing it
	// would uninstall the app.
	old := &unstructured.Unstructured{}
	old.SetGroupVersionKind(appClaimGVK)
	old.SetName(comp.Name)
	old.SetNamespace(comp.Namespace)
	_ = unstructured.SetNestedField(old.Object, "xwiki-ce", "spec", "profileRef", "name")
	_ = unstructured.SetNestedField(old.Object, "kept-by-crossplane", "spec", "resourceRef", "name")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(old).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", KernelRealm: "kernel"}
	zone := edgeZone{zoneNames: zoneNames{domain: "acme.k.example"}}

	ready, _, err := r.ensureAppClaim(ctx, comp, tenant, zone, pullSecrets{}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("a claim nothing has composed yet is ready")
	}
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(appClaimGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, claim); err != nil {
		t.Fatal(err)
	}
	spec := claim.Object["spec"].(map[string]interface{})
	if got, _, _ := unstructured.NestedString(spec, "profileRef", "name"); got != "odoo-base-ce" {
		t.Errorf("profileRef = %q", got)
	}
	if spec["tenantNamespace"] != "tenant-acme" || spec["domain"] != "acme.k.example" {
		t.Errorf("namespace and domain = %v %v", spec["tenantNamespace"], spec["domain"])
	}
	// The realm is the tenant's by the one rule, not the tenant's name
	// re-derived by the Composition.
	if spec["realm"] != keycloak.RealmName(tenant) {
		t.Errorf("realm = %v, want %q", spec["realm"], keycloak.RealmName(tenant))
	}
	named := tenant.DeepCopy()
	named.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "acme-people"}
	other := comp.DeepCopy()
	other.Name, other.UID = "wiki", "uid-wiki"
	if _, _, err := r.ensureAppClaim(ctx, other, named, zone, pullSecrets{}, "", false); err != nil {
		t.Fatal(err)
	}
	otherClaim := &unstructured.Unstructured{}
	otherClaim.SetGroupVersionKind(appClaimGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: other.Name, Namespace: other.Namespace}, otherClaim); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := unstructured.NestedString(otherClaim.Object, "spec", "realm"); got != "acme-people" {
		t.Errorf("realm of a tenant that names one = %q", got)
	}
	if addons, _, _ := unstructured.NestedStringSlice(spec, "addons"); strings.Join(addons, " ") != "crm-ce" {
		t.Errorf("addons = %v", addons)
	}
	if n, _, _ := unstructured.NestedInt64(spec, "config", "replicas"); n != 3 {
		t.Errorf("replicas = %d", n)
	}
	if v, _, _ := unstructured.NestedString(spec, "config", "extraValues", "web", "theme"); v != "dark" {
		t.Errorf("extraValues = %v", spec["config"])
	}
	// What Crossplane put on the claim is still there.
	if v, _, _ := unstructured.NestedString(spec, "resourceRef", "name"); v != "kept-by-crossplane" {
		t.Error("the claim's own fields were overwritten")
	}
	if !ownedBy(claim, comp) {
		t.Error("the claim is not owned by its component")
	}

	// Composed and deployed: ready, and a second pass changes nothing.
	_ = unstructured.SetNestedSlice(claim.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True"},
	}, "status", "conditions")
	if err := c.Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	ready, _, err = r.ensureAppClaim(ctx, comp, tenant, zone, pullSecrets{}, "", false)
	if err != nil || !ready {
		t.Fatalf("ready = %v, err = %v", ready, err)
	}

	// The addon is deactivated and the configuration dropped: the claim
	// follows, losing the fields rather than keeping the last value.
	comp.Spec.Addons, comp.Spec.Config = nil, nil
	if _, _, err := r.ensureAppClaim(ctx, comp, tenant, zone, pullSecrets{}, "", false); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, claim); err != nil {
		t.Fatal(err)
	}
	spec = claim.Object["spec"].(map[string]interface{})
	if _, has := spec["addons"]; has {
		t.Error("a deactivated addon is still on the claim")
	}
	if _, has := spec["config"]; has {
		t.Error("configuration that was withdrawn is still on the claim")
	}
}

// A claim another controller still holds is left to it: this component says
// it is waiting and writes nothing, rather than fighting over the object.
func TestAClaimSomethingElseControlsIsNotTaken(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "xwiki-ce", tenantNamespaceName(tenant), "uid-comp"
	comp.Spec.ProfileRef.Name = "xwiki-ce"

	yes := true
	held := &unstructured.Unstructured{}
	held.SetGroupVersionKind(appClaimGVK)
	held.SetName(comp.Name)
	held.SetNamespace(comp.Namespace)
	held.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "gentianos.io/v1alpha1", Kind: "XTenant", Name: "acme-x", UID: "uid-xtenant", Controller: &yes,
	}})
	_ = unstructured.SetNestedField(held.Object, "tenant-acme", "spec", "tenantNamespace")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(held).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	ready, message, err := r.ensureAppClaim(ctx, comp, tenant, edgeZone{zoneNames: zoneNames{domain: "acme.k.example"}}, pullSecrets{}, "", false)
	if err != nil || ready || !strings.Contains(message, "XTenant acme-x") {
		t.Fatalf("ready=%v message=%q err=%v", ready, message, err)
	}
	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(appClaimGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, after); err != nil {
		t.Fatal(err)
	}
	if ownedBy(after, comp) {
		t.Fatal("the claim was taken from its controller")
	}
	if _, has, _ := unstructured.NestedString(after.Object, "spec", "domain"); has {
		t.Fatal("the claim was written to while another controller held it")
	}
}
