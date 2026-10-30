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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/placement"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// profileFixture is a profile of the given classes; placedOnEvery gives it
// placement {tenants: every}, the way the desktop's profile says it.
func profileFixture(name string, placedOnEvery bool, classes ...gentianov1alpha1.ComponentClass) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	p.Spec.Classes = classes
	if placedOnEvery {
		p.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementEvery}
	}
	return p
}

// placedEntry is the entry of spec.apps the director's placement worker
// writes for a placeable profile.
func placedEntry(name string) gentianov1alpha1.TenantApp {
	return gentianov1alpha1.TenantApp{Profile: name, Placed: true}
}

// defaultPlaced is a Component an earlier release placed for a profile
// itself, labeled origin default, with no entry in spec.apps.
func defaultPlaced(tenant *gentianov1alpha1.Tenant, name string) *gentianov1alpha1.Component {
	c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: tenantNamespaceName(tenant),
		Labels: map[string]string{componentOriginLabel: componentOriginDefault},
	}}
	c.Spec.ProfileRef.Name = name
	c.Spec.Class = gentianov1alpha1.ComponentClassApp
	return c
}

func placementsOf(t *testing.T, r *TenantReconciler) placement.Answer {
	t.Helper()
	answer, err := r.Placements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// Every profile that declares a placement on every tenant is in the answer
// for both kinds of tenant; one placed on the platform tenant is in the
// platform's alone. Nothing is keyed on a name the operator knows: the
// desktop and the administration console are placed by the same rule, and
// a third such component needs no change here. A profile not certified for
// class app is placed nowhere, and an installable one is not placed. The
// deprecated flags of a catalog published before placement say the same.
func TestPlacementsAreWhatTheProfilesDeclare(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	console := profileFixture("operations-console", false, gentianov1alpha1.ComponentClassApp)
	console.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementPlatform}
	legacy := profileFixture("legacy-default", false, gentianov1alpha1.ComponentClassApp)
	legacy.Spec.DefaultForTenants = true
	legacyPlatform := profileFixture("legacy-platform", false, gentianov1alpha1.ComponentClassApp)
	legacyPlatform.Spec.DefaultForPlatform = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		profileFixture("desktop", true, gentianov1alpha1.ComponentClassApp),
		profileFixture("admin-console", true, gentianov1alpha1.ComponentClassApp),
		console, legacy, legacyPlatform,
		// Declared, but not certified for tenants: there is no tenant to give
		// a shared or system component to, so the declaration is ignored.
		profileFixture("registry", true, gentianov1alpha1.ComponentClassSharedApp),
		// Not declared: an ordinary app a tenant installs on purpose.
		profileFixture("nextcloud", false, gentianov1alpha1.ComponentClassApp),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme, KernelRealm: "kernel"}

	got := placementsOf(t, r)
	if want := []string{"admin-console", "desktop", "legacy-default"}; !reflect.DeepEqual(got.Users.Wanted, want) {
		t.Errorf("a user tenant has %v, want %v", got.Users.Wanted, want)
	}
	if want := []string{"admin-console", "desktop", "legacy-default", "legacy-platform", "operations-console"}; !reflect.DeepEqual(got.Platform.Wanted, want) {
		t.Errorf("the platform tenant has %v, want %v", got.Platform.Wanted, want)
	}
	if len(got.Users.Undecided) != 0 || len(got.Platform.Undecided) != 0 {
		t.Errorf("undecided = %v / %v, want none", got.Users.Undecided, got.Platform.Undecided)
	}
}

// A profile annotated as needing usage statistics is placed nowhere on a
// cluster that does not report, so the placement worker takes its entry
// out; with reporting on it is placed as declared.
func TestAProfileThatNeedsUsageStatisticsFollowsTheReport(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	console := profileFixture("operations-console", false, gentianov1alpha1.ComponentClassApp)
	console.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementPlatform}
	console.Annotations = map[string]string{placement.RequiresUsageStatisticsAnnotation: "true"}
	for _, reporting := range []bool{true, false} {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(console.DeepCopy()).Build()
		r := &TenantReconciler{Client: c, Scheme: scheme, KernelRealm: "kernel", UsageStatisticsEnabled: reporting}
		got := placementsOf(t, r)
		if have := got.Platform.Has("operations-console"); have != reporting {
			t.Fatalf("reporting %v: console placed = %v", reporting, have)
		}
		if got.Users.Has("operations-console") {
			t.Fatalf("reporting %v: a platform-only console is placed on user tenants", reporting)
		}
	}
}

// A placeable profile materialized from a catalog that has left the catalog
// directory -- its catalog renamed it, and the installer took the old name
// out -- is still an object, because the catalog's Application never
// prunes, and still declares itself placeable. It is placed no more: the
// Application marking it as requiring pruning is what says it left. Until it
// says so, or where it cannot be read, it stays placed.
func TestAPlaceableProfileThatLeftTheCatalogIsPlacedNoMore(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	old := profileFixture("operations-console", true, gentianov1alpha1.ComponentClassApp)
	old.Annotations = map[string]string{profilebundle.Annotation: "Ynl0ZXM="}
	renamed := profileFixture("operations-console-ee", true, gentianov1alpha1.ComponentClassApp)
	renamed.Annotations = map[string]string{profilebundle.Annotation: "Ynl0ZXM="}

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(profilebundle.ApplicationGVK)
	app.SetName(profilebundle.CatalogueApplicationPrefix + "prod")
	app.SetNamespace(layout.Namespace(layout.GitOps))
	resources := func(prunable bool) []interface{} {
		return []interface{}{
			map[string]interface{}{"group": "gentianos.io", "kind": "ComponentProfile", "name": "operations-console", "requiresPruning": prunable},
			map[string]interface{}{"group": "gentianos.io", "kind": "ComponentProfile", "name": "operations-console-ee"},
		}
	}
	_ = unstructured.SetNestedSlice(app.Object, resources(false), "status", "resources")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(old, renamed, app).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	want := []string{"operations-console", "operations-console-ee"}
	if got := placementsOf(t, r); !reflect.DeepEqual(got.Users.Wanted, want) || !reflect.DeepEqual(got.Platform.Wanted, want) {
		t.Fatalf("while both are declared: placements = %+v, want %v", got, want)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(app.Object, resources(true), "status", "resources")
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	want = []string{"operations-console-ee"}
	if got := placementsOf(t, r); !reflect.DeepEqual(got.Users.Wanted, want) || !reflect.DeepEqual(got.Platform.Wanted, want) {
		t.Fatalf("after the old name left the catalog: placements = %+v, want %v", got, want)
	}

	// No catalog Application to ask: nothing is known to be gone, and the
	// declaration is followed as before.
	bare := fake.NewClientBuilder().WithScheme(scheme).WithObjects(old.DeepCopy()).Build()
	rb := &TenantReconciler{Client: bare, Scheme: scheme}
	if got := placementsOf(t, rb); !got.Users.Has("operations-console") {
		t.Fatalf("without an Application: placements = %+v", got)
	}
}

// A placed entry of spec.apps becomes a Component like any install: named
// after its profile, of class app, labeled install, owned by the tenant so
// it goes when the tenant goes. When the placement worker takes the entry
// out, the Component goes with it -- the same path as an uninstall, and the
// only one: the operator decides no placement of its own.
func TestAPlacedEntryIsAComponentUntilItLeavesSpecApps(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := platformTenantFixture()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{placedEntry("desktop"), placedEntry("admin-console")}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		profileFixture("desktop", true, gentianov1alpha1.ComponentClassApp),
		profileFixture("admin-console", true, gentianov1alpha1.ComponentClassApp),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}
	ns := tenantNamespaceName(tenant)

	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if got := componentNames(t, c, ns); got != "admin-console desktop" {
		t.Fatalf("components = %q, want admin-console and desktop", got)
	}
	for _, name := range []string{"desktop", "admin-console"} {
		comp := &gentianov1alpha1.Component{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, comp); err != nil {
			t.Fatal(err)
		}
		if comp.Spec.ProfileRef.Name != name || comp.Spec.Class != gentianov1alpha1.ComponentClassApp {
			t.Fatalf("%s spec = %+v", name, comp.Spec)
		}
		if comp.Labels[componentOriginLabel] != componentOriginInstall {
			t.Fatalf("%s origin = %q, want install", name, comp.Labels[componentOriginLabel])
		}
		if len(comp.OwnerReferences) != 1 || comp.OwnerReferences[0].Name != tenant.Name {
			t.Fatalf("%s owners = %v", name, comp.OwnerReferences)
		}
	}

	// Idempotent: a second pass creates nothing and changes nothing.
	before := &gentianov1alpha1.Component{}
	_ = c.Get(ctx, types.NamespacedName{Name: "desktop", Namespace: ns}, before)
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	after := &gentianov1alpha1.Component{}
	_ = c.Get(ctx, types.NamespacedName{Name: "desktop", Namespace: ns}, after)
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatal("a second pass wrote the desktop again")
	}

	// The placement worker takes the console's entry out.
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{placedEntry("desktop")}
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if got := componentNames(t, c, ns); got != "desktop" {
		t.Fatalf("after the entry left: components = %q, want desktop only", got)
	}
}

// A Component an earlier release placed itself (labeled origin default) has
// no entry in spec.apps until the placement worker writes one. Until then it
// is left exactly as it is -- not deleted, because removing one purged its
// data, and not changed. So is a Component without the label at all. Once
// its entry appears, it is relabeled install, the same object, and from then
// on it is an install like any other.
func TestALegacyPlacedComponentIsLeftAloneUntilItsEntryAppears(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	ns := tenantNamespaceName(tenant)
	unlabeled := defaultPlaced(tenant, "legacy")
	unlabeled.Labels = nil
	legacyDesktop := defaultPlaced(tenant, "desktop")
	legacyDesktop.UID = "uid-legacy-desktop"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		profileFixture("desktop", true, gentianov1alpha1.ComponentClassApp),
		profileFixture("nextcloud", false, gentianov1alpha1.ComponentClassApp),
		legacyDesktop,
		unlabeled,
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	get := func(name string) *gentianov1alpha1.Component {
		t.Helper()
		comp := &gentianov1alpha1.Component{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, comp); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return comp
	}
	desktopBefore, legacyBefore := get("desktop"), get("legacy")

	// An install of something else, and no entry for the desktop.
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "nextcloud"}}
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if got := componentNames(t, c, ns); got != "desktop legacy nextcloud" {
		t.Fatalf("components = %q", got)
	}
	if d := get("desktop"); d.ResourceVersion != desktopBefore.ResourceVersion || d.Labels[componentOriginLabel] != componentOriginDefault {
		t.Fatalf("the legacy desktop was changed: labels %v", d.Labels)
	}
	if l := get("legacy"); l.ResourceVersion != legacyBefore.ResourceVersion {
		t.Fatal("the unlabeled component was changed")
	}

	// Its entry appears: the same object, relabeled.
	tenant.Spec.Apps = append(tenant.Spec.Apps, placedEntry("desktop"))
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	d := get("desktop")
	if d.Labels[componentOriginLabel] != componentOriginInstall {
		t.Fatalf("origin = %q, want install once its entry is there", d.Labels[componentOriginLabel])
	}
	if d.UID != desktopBefore.UID {
		t.Fatal("the legacy desktop was replaced rather than relabeled")
	}

	// From then on it goes with its entry.
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "nextcloud"}}
	if err := r.ensureAppComponents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if got := componentNames(t, c, ns); got != "legacy nextcloud" {
		t.Fatalf("after its entry left: components = %q", got)
	}
}

// A Component written before spec.tenancy became spec.class carries neither,
// and is repaired in place.
//
// Replacing it was the first attempt and it deadlocked a live cluster: the
// object has a finalizer, removing a finalizer is an update of the whole
// object, and the whole object is invalid while class is empty. The delete
// never completed and the create that followed answered AlreadyExists, so
// both shipped components sat terminating and un-finalizable. The
// immutability rule admits a transition from empty for exactly this repair.
func TestAComponentWrittenBeforeTheRenameIsRepaired(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := platformTenantFixture()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{placedEntry("desktop")}
	stale := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{
		Name: "desktop", Namespace: tenantNamespaceName(tenant),
	}}
	stale.Spec.ProfileRef.Name = "desktop"
	// No Class: what the rename leaves behind.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(), stale,
		profileFixture("desktop", true, gentianov1alpha1.ComponentClassApp),
	).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}
	if err := r.ensureAppComponents(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "desktop", Namespace: tenantNamespaceName(tenant),
	}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Class != gentianov1alpha1.ComponentClassApp {
		t.Fatalf("class = %q, want app: the stale component was not repaired", got.Spec.Class)
	}
	// Repaired, which means the same object: a replacement would have lost
	// the finalizer that could not have been removed in the first place.
	if got.UID != stale.UID {
		t.Fatal("the component was replaced rather than repaired")
	}
}
