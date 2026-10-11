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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

// nextcloudBase is a component that owns a host, as the catalogue converts one.
func nextcloudBaseProfile() *gentianov1alpha1.ComponentProfile {
	p := profileFixture("nextcloud-base-ce", false, gentianov1alpha1.ComponentClassApp)
	p.Spec.Launch = gentianov1alpha1.ComponentLaunchNone
	p.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	p.Spec.Version = "1.0.0"
	p.Spec.Package.Chart = &gentianov1alpha1.ChartRef{
		Repository: "oci://example.invalid/nextcloud", Name: "nextcloud", Version: "1.0.0",
	}
	p.Spec.Expose = []gentianov1alpha1.ExposureSpec{{
		Name: "web", Surface: gentianov1alpha1.SurfaceGateway,
		AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "cloud",
		Backend: gentianov1alpha1.BackendRef{Service: "nextcloud", Port: 8080},
	}}
	return p
}

// calendarAddon activates inside the base and publishes nothing of its own.
func calendarAddonProfile() *gentianov1alpha1.ComponentProfile {
	p := profileFixture("nextcloud-calendar-ce", false, gentianov1alpha1.ComponentClassApp)
	p.Spec.Launch = gentianov1alpha1.ComponentLaunchTile
	p.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	p.Spec.Version = "1.0.0"
	p.Spec.Package.Addon = &gentianov1alpha1.PackageAddon{ID: "calendar", Of: "nextcloud-base-ce"}
	p.Spec.Expose = []gentianov1alpha1.ExposureSpec{{
		Name: "calendar", Surface: gentianov1alpha1.SurfaceGateway,
		AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: "cloud",
		Backend: gentianov1alpha1.BackendRef{
			Component: "nextcloud-base-ce", Service: "nextcloud", Port: 8080,
		},
		Tile: &gentianov1alpha1.ExposureTile{
			DisplayName:  "Calendar",
			DisplayNames: map[string]string{"de_DE": "Kalender"},
			Logo:         testTileLogo,
			Path:         "/?app=calendar",
			Relation:     "can_launch",
		},
	}}
	return p
}

func componentFor(name, namespace string) *gentianov1alpha1.Component {
	return &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Finalizers: []string{componentFinalizer},
		},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: name},
			Class:      gentianov1alpha1.ComponentClassApp,
		},
	}
}

// An addon installs nothing and must not take its base's host.
//
// Its exposure names the base's Service, which is what says the tile opens the
// base. Writing a route for it would put a SECOND HTTPRoute on that hostname,
// both matching /, and Gateway API resolves that by picking one -- so a
// calendar addon could take cloud.<tenant> away from Nextcloud itself.
func TestAnAddonRoutesNothingAndInstallsNothing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	_ = networkingv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	ns := tenantNamespaceName(tenant)

	base := componentFor("nextcloud-base-ce", ns)
	base.Status.Conditions = []metav1.Condition{{
		Type: conditionComponentReady, Status: metav1.ConditionTrue,
		Reason: "Ready", LastTransitionTime: metav1.Now(),
	}}
	addon := componentFor("nextcloud-calendar-ce", ns)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), nextcloudBaseProfile(), calendarAddonProfile(), base, addon).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(addon)}); err != nil {
		t.Fatal(err)
	}

	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(addon), got); err != nil {
		t.Fatal(err)
	}
	ready := componentReadyCondition(got)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("addon condition = %+v, want Ready", ready)
	}
	// No route of its own: the base serves that host.
	routes := &gatewayv1.HTTPRouteList{}
	if err := c.List(context.Background(), routes, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	for _, rt := range routes.Items {
		if rt.Name == "nextcloud-calendar-ce-calendar" {
			t.Fatalf("the addon wrote a route on its base's host: %s", rt.Name)
		}
	}
	// And no Release: an addon deploys nothing.
	releases := &gentianov1alpha1.ComponentList{}
	_ = c.List(context.Background(), releases, client.InNamespace(ns))
}

// An addon whose base is not installed is not Ready. A tile pointing at a host
// nobody serves is worse than no tile.
func TestAnAddonWaitsForItsBase(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	addon := componentFor("nextcloud-calendar-ce", tenantNamespaceName(tenant))

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), calendarAddonProfile(), addon).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(addon)}); err != nil {
		t.Fatal(err)
	}
	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(addon), got); err != nil {
		t.Fatal(err)
	}
	ready := componentReadyCondition(got)
	if ready == nil || ready.Reason != "AddonBaseNotReady" {
		t.Fatalf("condition = %+v, want AddonBaseNotReady", ready)
	}
}

// An API entry runs nothing here and is Ready saying where it points.
func TestAnAPIEntryIsReadyWithoutRunningAnything(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	tenant := acmeTenantFixture()

	profile := profileFixture("gentian-subscriptions-me", false, gentianov1alpha1.ComponentClassApp)
	profile.Spec.Launch = gentianov1alpha1.ComponentLaunchNone
	profile.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	profile.Spec.Version = "1.0.0"
	profile.Spec.Package.API = &gentianov1alpha1.APIIntegration{
		Runtime: gentianov1alpha1.APIIntegrationRuntimeRedirect,
		BaseURL: "https://corp.desk.gentian.org",
	}
	comp := componentFor("gentian-subscriptions-me", tenantNamespaceName(tenant))

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), profile, comp).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(comp)}); err != nil {
		t.Fatal(err)
	}
	got := &gentianov1alpha1.Component{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(comp), got); err != nil {
		t.Fatal(err)
	}
	ready := componentReadyCondition(got)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v, want Ready", ready)
	}
	if want := "corp.desk.gentian.org"; !strings.Contains(ready.Message, want) {
		t.Fatalf("message %q does not say where it points", ready.Message)
	}
}

// The tile an addon declares appears, on its BASE's host.
//
// This is the whole point of the addon path. Twenty catalogue profiles carry
// exactly one user-visible thing — a tile deep-linking into the app they
// activate inside — and until the projection followed backend.component to the
// base's route, every one of them had a tile declared and no hostname to build
// a URL from, so none of them appeared.
func TestAnAddonsTileIsProjectedOnItsBasesHost(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	_ = corev1.AddToScheme(scheme)
	tenant := tileFixtureTenant("demo")
	ns := "tenant-demo"

	// The base's route, which is what actually serves cloud.demo.k.example.
	baseRoute := tileFixtureRoute("nextcloud-base-ce-web", ns, "cloud.demo.k.example")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant,
		nextcloudBaseProfile(), calendarAddonProfile(),
		componentFor("nextcloud-base-ce", ns), componentFor("nextcloud-calendar-ce", ns),
		baseRoute,
	).Build()
	r := &TileProjectionReconciler{Client: c}

	tiles, err := r.componentTiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var calendar *tilecatalogue.Tile
	for i := range tiles {
		if tiles[i].DisplayName == "Calendar" {
			calendar = &tiles[i]
		}
	}
	if calendar == nil {
		t.Fatalf("the addon's tile was not projected; got %d tile(s)", len(tiles))
	}
	if want := "https://cloud.demo.k.example/?app=calendar"; calendar.URL != want {
		t.Fatalf("tile URL = %q, want the base's host with the addon's path", calendar.URL)
	}
	if calendar.DisplayNames["de_DE"] != "Kalender" {
		t.Fatalf("the German label did not travel: %v", calendar.DisplayNames)
	}
}
