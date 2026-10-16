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
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// holdWorld is a tenant with one app that has an entry on the authenticated
// Gateway and a surface published from the DMZ, beside another tenant's
// install of the same app.
type holdWorld struct {
	c       client.Client
	r       *ComponentReconciler
	comp    *gentianov1alpha1.Component
	tenant  *gentianov1alpha1.Tenant
	entry   types.NamespacedName
	surface types.NamespacedName
	other   types.NamespacedName
	rules   map[types.NamespacedName][]gatewayv1.HTTPRouteRule
}

func newHoldWorld(t *testing.T) *holdWorld {
	t.Helper()
	ctx := context.Background()
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	ns := tenantNamespaceName(tenant)
	comp := componentFor("nextcloud-base-ce", ns)
	comp.Spec.Exposures = enabledShares(time.Now().Add(720 * time.Hour))
	profile := nextcloudWithPerimeter()
	otherComp := componentFor("nextcloud-base-ce", "tenant-other")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&gentianov1alpha1.TenantRestore{}).
		WithObjects(tenant.DeepCopy(), profile, comp, otherComp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
	if n, err := r.ensurePerimeter(ctx, comp, profile, tenant, r.zoneOf(tenant)); err != nil || n != 1 {
		t.Fatalf("published %d surfaces: %v", n, err)
	}
	e := &gentianov1alpha1.ExposureSpec{Name: "web", AuthMode: gentianov1alpha1.AuthModeOIDC, Paths: []string{"/apps"},
		Backend: gentianov1alpha1.BackendRef{Service: "nextcloud", Port: 8080}}
	authz := routeAuthz{relation: "can_use", object: "app:acme/nextcloud-base-ce"}
	for _, owner := range []*gentianov1alpha1.Component{comp, otherComp} {
		route := buildExposureRoute(owner, owner.Name+"-web", "files."+owner.Namespace+".example", r.zoneOf(tenant), e, authz, "k.example", nil)
		if err := ensureHTTPRouteResource(ctx, c, route); err != nil {
			t.Fatal(err)
		}
	}
	w := &holdWorld{
		c: c, r: r, comp: comp, tenant: tenant,
		entry:   types.NamespacedName{Namespace: ns, Name: comp.Name + "-web"},
		surface: types.NamespacedName{Namespace: layout.TenantDMZ(tenant.Name), Name: perimeterName(comp, "shares")},
		other:   types.NamespacedName{Namespace: "tenant-other", Name: otherComp.Name + "-web"},
		rules:   map[types.NamespacedName][]gatewayv1.HTTPRouteRule{},
	}
	for _, key := range []types.NamespacedName{w.entry, w.surface, w.other} {
		w.rules[key] = w.route(t, key).Spec.Rules
	}
	return w
}

func (w *holdWorld) route(t *testing.T, key types.NamespacedName) *gatewayv1.HTTPRoute {
	t.Helper()
	route := &gatewayv1.HTTPRoute{}
	if err := w.c.Get(context.Background(), key, route); err != nil {
		t.Fatalf("route %s: %v", key, err)
	}
	return route
}

func (w *holdWorld) restore(t *testing.T, phase gentianov1alpha1.TenantExportPhase, held ...string) *gentianov1alpha1.TenantRestore {
	t.Helper()
	ctx := context.Background()
	restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{
		Name: "r1", Namespace: w.comp.Namespace, Finalizers: []string{restoreFinalizer}}}
	if err := w.c.Create(ctx, restore); err != nil {
		t.Fatal(err)
	}
	restore.Status.Phase = phase
	restore.Status.HeldBack = held
	if err := w.c.Status().Update(ctx, restore); err != nil {
		t.Fatal(err)
	}
	return restore
}

func (w *holdWorld) setPhase(t *testing.T, restore *gentianov1alpha1.TenantRestore, phase gentianov1alpha1.TenantExportPhase) {
	t.Helper()
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(restore), restore); err != nil {
		t.Fatal(err)
	}
	restore.Status.Phase = phase
	if err := w.c.Status().Update(context.Background(), restore); err != nil {
		t.Fatal(err)
	}
}

func (w *holdWorld) applyHold(t *testing.T) bool {
	t.Helper()
	held, err := w.r.restoreHold(context.Background(), w.comp)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// assertHeld says every rule of the route answers with the hold and sends
// nowhere, matching what it matched before.
func (w *holdWorld) assertHeld(t *testing.T, key types.NamespacedName) {
	t.Helper()
	route := w.route(t, key)
	if !routeHeld(route) {
		t.Fatalf("%s is not held", key)
	}
	before := w.rules[key]
	if len(route.Spec.Rules) != len(before) {
		t.Fatalf("%s has %d rules held, had %d", key, len(route.Spec.Rules), len(before))
	}
	for i, rule := range route.Spec.Rules {
		if len(rule.BackendRefs) != 0 {
			t.Errorf("%s rule %d still sends to %v", key, i, rule.BackendRefs)
		}
		if len(rule.Filters) != 1 || rule.Filters[0].ExtensionRef == nil || rule.Filters[0].ExtensionRef.Name != restoreHoldFilterName ||
			string(rule.Filters[0].ExtensionRef.Kind) != "HTTPRouteFilter" {
			t.Errorf("%s rule %d is not answered by the hold: %+v", key, i, rule.Filters)
		}
		if !equality.Semantic.DeepEqual(rule.Matches, before[i].Matches) {
			t.Errorf("%s rule %d matches %+v, matched %+v", key, i, rule.Matches, before[i].Matches)
		}
	}
	filter := &unstructured.Unstructured{}
	filter.SetGroupVersionKind(httpRouteFilterGVK)
	if err := w.c.Get(context.Background(), types.NamespacedName{Namespace: key.Namespace, Name: restoreHoldFilterName}, filter); err != nil {
		t.Fatalf("no hold answer in %s: %v", key.Namespace, err)
	}
	if code, _, _ := unstructured.NestedInt64(filter.Object, "spec", "directResponse", "statusCode"); code != 503 {
		t.Errorf("the hold answers %d", code)
	}
	headers, _, _ := unstructured.NestedSlice(filter.Object, "spec", "directResponse", "header", "set")
	if !strings.Contains(strings.ToLower(strings.Join(func() []string {
		var out []string
		for _, h := range headers {
			out = append(out, h.(map[string]interface{})["name"].(string))
		}
		return out
	}(), ",")), "retry-after") {
		t.Errorf("the hold says no Retry-After: %v", headers)
	}
}

func (w *holdWorld) assertOpen(t *testing.T, key types.NamespacedName) {
	t.Helper()
	route := w.route(t, key)
	if routeHeld(route) {
		t.Fatalf("%s is still held", key)
	}
	if !equality.Semantic.DeepEqual(route.Spec.Rules, w.rules[key]) {
		t.Fatalf("%s rules are %+v, were %+v", key, route.Spec.Rules, w.rules[key])
	}
}

// A running restore that holds the app back holds every route of it, its
// entries and its published surfaces, and no other tenant's install of the
// same app. Ending lifts it, and the routes are what they were.
func TestARestoreHoldsTheAppsRoutesUntilItEnds(t *testing.T) {
	for _, end := range []gentianov1alpha1.TenantExportPhase{gentianov1alpha1.TenantExportPhaseReady, gentianov1alpha1.TenantExportPhaseFailed} {
		t.Run(string(end), func(t *testing.T) {
			w := newHoldWorld(t)
			restore := w.restore(t, gentianov1alpha1.TenantExportPhaseRunning, w.comp.Spec.ProfileRef.Name)
			if !w.applyHold(t) {
				t.Fatal("the app is not held")
			}
			w.assertHeld(t, w.entry)
			w.assertHeld(t, w.surface)
			w.assertOpen(t, w.other)

			// Writing the routes again while held keeps them held.
			if _, err := w.r.ensurePerimeter(context.Background(), w.comp, nextcloudWithPerimeter(), w.tenant, w.r.zoneOf(w.tenant)); err != nil {
				t.Fatal(err)
			}
			w.assertHeld(t, w.surface)

			w.setPhase(t, restore, end)
			if w.applyHold(t) {
				t.Fatal("an ended restore still holds the app")
			}
			w.assertOpen(t, w.entry)
			w.assertOpen(t, w.surface)
		})
	}
}

// Deleting a running restore, which is how one is stopped, lifts the hold
// while its finalizer still keeps it.
func TestDeletingARestoreLiftsTheHold(t *testing.T) {
	w := newHoldWorld(t)
	restore := w.restore(t, gentianov1alpha1.TenantExportPhaseRunning, w.comp.Spec.ProfileRef.Name)
	w.applyHold(t)
	w.assertHeld(t, w.entry)
	if err := w.c.Delete(context.Background(), restore); err != nil {
		t.Fatal(err)
	}
	if w.applyHold(t) {
		t.Fatal("a restore being deleted still holds the app")
	}
	w.assertOpen(t, w.entry)
	w.assertOpen(t, w.surface)
}

// A restore that does not name the app, or has not reached it, holds nothing.
func TestARestoreHoldsOnlyTheAppsItNames(t *testing.T) {
	w := newHoldWorld(t)
	w.restore(t, gentianov1alpha1.TenantExportPhaseRunning, "some-other-app")
	if w.applyHold(t) {
		t.Fatal("held an app the restore does not name")
	}
	w.assertOpen(t, w.entry)
	w.assertOpen(t, w.surface)
}

// A route the reconciler writes when the app is not held loses the hold's
// annotation, so a route never says it is held when it is not.
func TestWritingARouteThatIsNotHeldRemovesTheHold(t *testing.T) {
	w := newHoldWorld(t)
	ctx := context.Background()
	route := w.route(t, w.entry)
	if err := holdRoute(route); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Update(ctx, route); err != nil {
		t.Fatal(err)
	}
	desired := w.route(t, w.entry)
	delete(desired.Annotations, restoreHoldAnnotation)
	desired.Spec.Rules = w.rules[w.entry]
	if err := ensureHTTPRouteResource(ctx, w.c, desired); err != nil {
		t.Fatal(err)
	}
	w.assertOpen(t, w.entry)
}

// The component reconciler is woken when what a restore holds back changes,
// and not by every other status write of a running restore.
func TestOnlyAChangeInWhatIsHeldWakesTheComponents(t *testing.T) {
	p := restoreHoldChanged()
	running := &gentianov1alpha1.TenantRestore{Status: gentianov1alpha1.TenantRestoreStatus{
		Phase: gentianov1alpha1.TenantExportPhaseRunning, HeldBack: []string{"wiki"}}}
	progressed := running.DeepCopy()
	progressed.Status.Notes = []string{"something"}
	ended := running.DeepCopy()
	ended.Status.Phase = gentianov1alpha1.TenantExportPhaseReady
	more := running.DeepCopy()
	more.Status.HeldBack = append(more.Status.HeldBack, "drive")
	if p.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: progressed}) {
		t.Error("a status write that holds nothing new woke the components")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: ended}) {
		t.Error("a restore that ended did not wake the components")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: more}) {
		t.Error("a restore that holds another app did not wake the components")
	}
}

// A restore names the app as held back before anything of it is changed, and
// waits for the edge to hold it: nothing is paused and no secret is touched
// while a route of it still serves. Once the edge holds it the restore goes
// on, and when it ends nothing is named any more.
func TestARestoreWaitsForTheEdgeToHoldTheAppBack(t *testing.T) {
	w := newRestoreWorld(t, func(r *gentianov1alpha1.TenantRestore) { r.Spec.Apps = []string{"wiki"} })
	ctx := context.Background()
	comp := componentFor("wiki", "tenant-demo")
	route := buildExposureRoute(comp, "wiki-web", "wiki.demo.example", edgeZone{},
		&gentianov1alpha1.ExposureSpec{Name: "web", AuthMode: gentianov1alpha1.AuthModeOIDC,
			Backend: gentianov1alpha1.BackendRef{Service: "wiki", Port: 8080}},
		routeAuthz{}, "k.example", nil)
	for _, obj := range []client.Object{comp, route, deployment("wiki", "wiki", 1)} {
		if err := w.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatal(err)
		}
	}
	got := w.restore(t)
	if !got.HoldsBack("wiki") {
		t.Fatalf("wiki is not held back: %+v", got.Status.HeldBack)
	}
	entry := appStatus(&got.Status.Apps, "wiki")
	if entry.QuiesceStart != nil || len(got.Status.Quiesced) > 0 || *getDeployment(t, w.c, "wiki").Spec.Replicas != 1 {
		t.Fatal("the app was paused while the edge still served it")
	}
	if !strings.Contains(entry.Message, "tenant-demo/wiki-web") {
		t.Errorf("the restore does not say which route it waits for: %q", entry.Message)
	}

	// The component reconciler's part.
	held := &gatewayv1.HTTPRoute{}
	if err := w.c.Get(ctx, client.ObjectKeyFromObject(route), held); err != nil {
		t.Fatal(err)
	}
	if err := holdRoute(held); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Update(ctx, held); err != nil {
		t.Fatal(err)
	}
	got = w.run(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	if len(got.Status.HeldBack) != 0 || got.HoldsBack("wiki") {
		t.Fatalf("an ended restore still names %v", got.Status.HeldBack)
	}
	if appStatus(&got.Status.Apps, "wiki").RolloutStart == nil {
		t.Error("the restore did not wait for the app to roll out")
	}
}

// An edge that never holds the app back fails the restore before anything
// of the app is changed, and the hold it asked for ends with it.
func TestARestoreGivesUpOnAnEdgeThatDoesNotHoldTheAppBack(t *testing.T) {
	w := newRestoreWorld(t, func(r *gentianov1alpha1.TenantRestore) { r.Spec.Apps = []string{"wiki"} })
	ctx := context.Background()
	comp := componentFor("wiki", "tenant-demo")
	route := buildExposureRoute(comp, "wiki-web", "wiki.demo.example", edgeZone{},
		&gentianov1alpha1.ExposureSpec{Name: "web", AuthMode: gentianov1alpha1.AuthModeOIDC,
			Backend: gentianov1alpha1.BackendRef{Service: "wiki", Port: 8080}},
		routeAuthz{}, "k.example", nil)
	for _, obj := range []client.Object{comp, route, deployment("wiki", "wiki", 1)} {
		if err := w.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := w.r.Reconcile(ctx, ctrl.Request{NamespacedName: w.key}); err != nil {
			t.Fatal(err)
		}
	}
	got := w.restore(t)
	appStatus(&got.Status.Apps, "wiki").HeldBackAt = &metav1.Time{Time: time.Now().Add(-restoreHoldTimeout - time.Second)}
	if err := w.c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = w.run(t)
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseFailed {
		t.Fatalf("phase = %s", got.Status.Phase)
	}
	if got.HoldsBack("wiki") {
		t.Error("a failed restore still holds the app back")
	}
	if appStatus(&got.Status.Apps, "wiki").QuiesceStart != nil || *getDeployment(t, w.c, "wiki").Spec.Replicas != 1 {
		t.Error("the app was paused although the edge never held it back")
	}
}
