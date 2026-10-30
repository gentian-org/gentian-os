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
	"slices"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// appClaimOf is the App claim of a component's name in its namespace, with
// an owner reference of the given kind and name, the controller one or not.
func appClaimOf(comp *gentianov1alpha1.Component, kind, name string, controller bool) *unstructured.Unstructured {
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(appClaimGVK)
	claim.SetName(comp.Name)
	claim.SetNamespace(comp.Namespace)
	if kind != "" {
		uid := types.UID("uid-owner")
		// The component's own claim names the component by its UID, as an
		// owner reference does.
		if kind == "Component" && name == comp.Name {
			if comp.UID == "" {
				comp.UID = types.UID("uid-" + comp.Name)
			}
			uid = comp.UID
		}
		claim.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "gentianos.io/v1alpha1", Kind: kind, Name: name, UID: uid,
			Controller: &controller,
		}})
	}
	return claim
}

// directProfile is a chart profile that needs nothing of the Composition
// any more: generated secrets and a database, both rendered here now.
func directProfile() *gentianov1alpha1.ComponentProfile {
	p := chartProfileFixture("oci://registry.vendor.example/acme/charts")
	p.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Generated: []gentianov1alpha1.AppSecret{{Name: "key", ValuePath: "app.key"}}}
	p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Database: &gentianov1alpha1.DatabaseRequirement{}}}
	return p
}

func getComponent(t *testing.T, c client.Client, comp *gentianov1alpha1.Component) *gentianov1alpha1.Component {
	t.Helper()
	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(comp), got); err != nil {
		t.Fatal(err)
	}
	return got
}

// An app the Composition installed stays with it when its profile stops
// needing the Composition: moving it here would install it a second time
// under another release name. The first pass finds its owned claim and no
// release of this reconciler's, marks the component, and from then on the
// release name, the pod selector and the database egress are the
// Composition's. A later pass, the profile direct again and the claim
// briefly gone, still answers composed from the mark alone and writes
// nothing.
func TestAnAppTheCompositionInstalledStaysComposedWhenItsProfileNoLongerNeedsIt(t *testing.T) {
	ctx := context.Background()
	scheme := uninstallScheme(t)
	profile := directProfile()
	if composedDelivery(profile) {
		t.Fatal("the fixture is meant to be a profile delivered directly")
	}
	comp := componentFor("wiki", "tenant-acme")
	claim := appClaimOf(comp, "Component", "wiki", true)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, claim).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	tenant := acmeTenantFixture()
	direct := r.componentEgressNamespaces(componentFor("wiki", "tenant-acme"), profile, tenant)
	if !slices.Contains(direct, r.componentDatabaseNamespace(tenant, profile)) {
		t.Fatalf("a direct component's egress %v lacks its database's namespace", direct)
	}

	live := getComponent(t, c, comp)
	composed, err := r.deliversComposed(ctx, live, profile)
	if err != nil || !composed {
		t.Fatalf("composed = %v, err %v; want the Composition's install kept", composed, err)
	}
	got := getComponent(t, c, comp)
	if got.Annotations[deliveredByAnnotation] != deliveredByComposition {
		t.Fatalf("annotations = %v, want the component marked", got.Annotations)
	}
	if name := helmReleaseOf(got, profile); name != "wiki-release" {
		t.Fatalf("release = %q, want the Composition's", name)
	}
	if pods := componentPods(got, profile); !equalLabels(pods, map[string]string{appLabel: "wiki"}) {
		t.Fatalf("pods = %v, want selected by the app label", pods)
	}
	if egress := r.componentEgressNamespaces(got, profile, tenant); slices.Contains(egress, r.componentDatabaseNamespace(tenant, profile)) {
		t.Fatalf("egress %v opens the database the Composition opens itself", egress)
	}

	// The profile needs the Composition for a while, then not again; the
	// claim is gone meanwhile, so only the mark can answer.
	profile.Spec.Hooks = &gentianov1alpha1.HookSpec{}
	if composed, err := r.deliversComposed(ctx, got, profile); err != nil || !composed {
		t.Fatalf("with hooks: composed = %v, err %v", composed, err)
	}
	profile.Spec.Hooks = nil
	if err := c.Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	before := got.ResourceVersion
	if composed, err := r.deliversComposed(ctx, got, profile); err != nil || !composed {
		t.Fatalf("second pass: composed = %v, err %v; want the mark to keep it", composed, err)
	}
	if after := getComponent(t, c, comp).ResourceVersion; after != before {
		t.Fatalf("a marked component was written again (%s -> %s)", before, after)
	}
}

// Only the component's own claim is an install of it: one nobody controls,
// one something else controls, or one another component controls says
// nothing about this component. Nor does its own claim where a release of
// this reconciler's is there beside it: the app runs direct. None of these
// marks the component.
func TestOnlyTheComponentsOwnClaimWithoutADirectReleaseMakesItComposed(t *testing.T) {
	ctx := context.Background()
	profile := directProfile()
	for name, objects := range map[string]func(*gentianov1alpha1.Component) []client.Object{
		"a claim with no owner": func(comp *gentianov1alpha1.Component) []client.Object {
			return []client.Object{appClaimOf(comp, "", "", false)}
		},
		"a claim the component owns but does not control": func(comp *gentianov1alpha1.Component) []client.Object {
			return []client.Object{appClaimOf(comp, "Component", comp.Name, false)}
		},
		"a claim something else controls": func(comp *gentianov1alpha1.Component) []client.Object {
			return []client.Object{appClaimOf(comp, "Tenant", comp.Name, true)}
		},
		"a claim another component controls": func(comp *gentianov1alpha1.Component) []client.Object {
			return []client.Object{appClaimOf(comp, "Component", "other", true)}
		},
		"its own claim beside a direct release": func(comp *gentianov1alpha1.Component) []client.Object {
			return []client.Object{appClaimOf(comp, "Component", comp.Name, true), directRelease(releaseName(comp), nil)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			scheme := uninstallScheme(t)
			comp := componentFor("wiki", "tenant-acme")
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects(comp), comp)...).Build()
			r := &ComponentReconciler{Client: c, Scheme: scheme}
			composed, err := r.deliversComposed(ctx, getComponent(t, c, comp), profile)
			if err != nil || composed {
				t.Fatalf("composed = %v, err %v; want direct", composed, err)
			}
			if got := getComponent(t, c, comp); got.Annotations[deliveredByAnnotation] != "" {
				t.Fatalf("annotations = %v, want none", got.Annotations)
			}
		})
	}
}

// A new install whose profile needs nothing of the Composition is direct
// and is never marked, however often it is reconciled. A new install whose
// profile does need it is composed, but is not marked before its claim
// exists: a mark with nothing installed would hold a component that never
// reached the Composition.
func TestANewInstallIsDeliveredByItsProfileAndNotMarked(t *testing.T) {
	ctx := context.Background()
	scheme := uninstallScheme(t)
	comp := componentFor("wiki", "tenant-acme")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	profile := directProfile()
	for pass := 0; pass < 2; pass++ {
		got := getComponent(t, c, comp)
		if composed, err := r.deliversComposed(ctx, got, profile); err != nil || composed {
			t.Fatalf("pass %d: composed = %v, err %v; want direct", pass, composed, err)
		}
		if helmReleaseOf(got, profile) != releaseName(comp) {
			t.Fatalf("release = %q, want the direct one", helmReleaseOf(got, profile))
		}
	}
	profile.Spec.Hooks = &gentianov1alpha1.HookSpec{}
	if composed, err := r.deliversComposed(ctx, getComponent(t, c, comp), profile); err != nil || !composed {
		t.Fatalf("with hooks: composed = %v, err %v", composed, err)
	}
	if got := getComponent(t, c, comp); got.Annotations[deliveredByAnnotation] != "" {
		t.Fatalf("annotations = %v, want none on an install that has no claim yet", got.Annotations)
	}
}

// A contract selects an app the Composition installed by the app label its
// pods carry, even before its Component has been reconciled and marked:
// its owned claim and the absence of a direct release say so. Selecting it
// by a release label would let the contract through to no pod at all.
func TestAContractSelectsAnAppTheCompositionInstalledByItsAppLabelBeforeItIsMarked(t *testing.T) {
	ctx := context.Background()
	scheme := appStoreScheme(t)

	desktop := chartProfileFixture("oci://registry.example/gentian")
	desktop.Name = "desktop"
	desktop.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform
	desktop.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementEvery}
	desktop.Spec.Provides = []gentianov1alpha1.ContractRef{{
		Name: "desktop-api", Protocol: "http",
		Endpoint: &gentianov1alpha1.ContractEndpoint{Service: "desktop-gentian-portal-api", Port: 8000},
	}}
	store := mappingProfile(map[string]gentianov1alpha1.IntegrationValueMapping{
		"desktop-api": {EndpointKey: "api.env.DESKTOP_API_URL"},
	})
	store.Name = "store"
	store.Spec.Integrations = []gentianov1alpha1.IntegrationRef{{Contract: "desktop-api", Provider: "desktop", Capabilities: []string{"tiles"}}}
	if composedDelivery(store) {
		t.Fatal("the store's profile is meant to need nothing of the Composition")
	}

	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "uid-acme"}}
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{placedEntry("desktop"), {Profile: "store"}}
	tenant.Spec.Grants = []gentianov1alpha1.TenantAppGrant{{AppGrantSpec: gentianov1alpha1.AppGrantSpec{
		App: "store", Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "desktop-api", Granted: []string{"tiles"}}},
	}}}
	ns := tenantNamespaceName(tenant)
	storeComp := componentFor("store", ns)
	claim := appClaimOf(storeComp, "Component", "store", true)
	vault := memVault{secrets.ContractPath(tenant.Name, "desktop-api"): {"password": "shared", "endpoint": "", "user": ""}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), desktop, store, storeComp, claim).
		WithStatusSubresource(&gentianov1alpha1.IntegrationBinding{}).Build()
	tr := &TenantReconciler{Client: c, Scheme: scheme, Seeder: secrets.NewSeeder(vault, nil)}
	if err := tr.ensureAppGrants(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	bindings, err := tr.collectDesiredIntegrationBindings(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want the store's one", len(bindings))
	}
	if err := c.Create(ctx, bindings[0]); err != nil {
		t.Fatal(err)
	}
	if err := tr.ensureNetworkPolicies(ctx, tenant); err != nil {
		t.Fatal(err)
	}

	out := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "contract-" + bindings[0].Name, Namespace: ns}, out); err != nil {
		t.Fatalf("no way out for the consumer: %v", err)
	}
	if want := map[string]string{appLabel: "store"}; !equalLabels(out.Spec.PodSelector.MatchLabels, want) {
		t.Fatalf("consumer selected by %v, want %v", out.Spec.PodSelector.MatchLabels, want)
	}
	// The desktop is still direct and selected by its release.
	if len(out.Spec.Egress) != 1 || len(out.Spec.Egress[0].To) != 1 ||
		!equalLabels(out.Spec.Egress[0].To[0].PodSelector.MatchLabels, map[string]string{componentInstanceLabel: ns + "-desktop"}) {
		t.Fatalf("consumer egress = %+v", out.Spec.Egress)
	}
}

// A claim of the component's name that is on its way out, or that an earlier
// component of the same name owned, does not make a reinstall the
// Composition's for good: the mark is set only for the component's own,
// live claim.
func TestAClaimGoingOrOfAnEarlierInstallDoesNotMarkAReinstall(t *testing.T) {
	ctx := context.Background()
	scheme := uninstallScheme(t)
	profile := directProfile()

	comp := componentFor("wiki", "tenant-acme")
	comp.UID = "uid-now"
	earlier := appClaimOf(&gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-acme", UID: "uid-before"}}, "Component", "wiki", true)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, earlier).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	if composed, err := r.deliversComposed(ctx, comp, profile); err != nil || composed {
		t.Fatalf("an earlier install's claim: composed %v, err %v; want direct", composed, err)
	}

	going := appClaimOf(comp, "Component", "wiki", true)
	now := metav1.Now()
	going.SetDeletionTimestamp(&now)
	going.SetFinalizers([]string{"finalizer.apiextensions.crossplane.io"})
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp, going).Build()
	r = &ComponentReconciler{Client: c, Scheme: scheme}
	if composed, err := r.deliversComposed(ctx, comp, profile); err != nil || composed {
		t.Fatalf("a claim on its way out: composed %v, err %v; want direct", composed, err)
	}
	if getComponent(t, c, comp).Annotations[deliveredByAnnotation] != "" {
		t.Fatal("a reinstall was marked as the Composition's")
	}
}

// A chart is not installed here beside an App claim of its name that is not
// the component's own -- nobody controls it, or something else does: what
// that claim composed names the same app. The component waits and says why.
func TestADirectChartWaitsForAClaimThatIsNotItsOwn(t *testing.T) {
	ctx := context.Background()
	scheme := uninstallScheme(t)
	comp := componentFor("wiki", "tenant-acme")
	comp.UID = "uid-now"
	for name, claim := range map[string]*unstructured.Unstructured{
		"nobody's":       appClaimOf(comp, "", "", false),
		"the tenant's":   appClaimOf(comp, "Tenant", "acme", true),
		"its own":        appClaimOf(comp, "Component", "wiki", true),
		"an earlier one": appClaimOf(&gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-acme", UID: "uid-before"}}, "Component", "wiki", true),
	} {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
		foreign, err := foreignClaim(ctx, c, comp)
		if err != nil {
			t.Fatal(err)
		}
		if (name == "its own") != (foreign == "") {
			t.Errorf("%s claim: foreign = %q", name, foreign)
		}
	}
}
