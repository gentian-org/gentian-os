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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func exposeAt(name, sub string, surface gentianov1alpha1.SurfaceKind) gentianov1alpha1.ExposureSpec {
	return gentianov1alpha1.ExposureSpec{
		Name: name, Surface: surface, AuthMode: gentianov1alpha1.AuthModeOIDC, SubDomain: sub,
		Backend: gentianov1alpha1.BackendRef{Service: "s", Port: 80},
	}
}

func zoneHostsOf(t *testing.T, r *TileProjectionReconciler, tenant string) []string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Name: zoneHostsConfigMapName(tenant), Namespace: layout.Namespace(layout.Control),
	}
	if err := r.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("no projection for %s: %v", tenant, err)
	}
	if cm.Data[zoneHostsKey] == "" {
		return nil
	}
	return strings.Split(cm.Data[zoneHostsKey], "\n")
}

// The zone's Keycloak client admits a redirect only to a host it lists, and
// that list was written in the Composition by hand. A component declaring a
// subDomain of its own got "Invalid parameter: redirect_uri" on a page naming
// neither the component nor a list. The operator knows every host, because it
// composes the routes, so it projects them.
func TestZoneHostsFollowTheComponentsAndNotAList(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := platformTenantFixture()
	wiki := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "wiki"}}
	wiki.Spec.Expose = []gentianov1alpha1.ExposureSpec{
		exposeAt("web", "wiki", gentianov1alpha1.SurfaceGateway),
		// A second entry on the same host must not double it.
		exposeAt("api", "wiki", gentianov1alpha1.SurfaceGateway),
		// A perimeter surface is published through its own proxy with its own
		// credential and never reaches the zone's client, so listing it would
		// widen that client for a host the zone does not serve.
		exposeAt("share", "public", gentianov1alpha1.SurfacePerimeter),
	}
	// No subDomain: the route is served on the component's own name.
	unnamed := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "notes"}}
	unnamed.Spec.Expose = []gentianov1alpha1.ExposureSpec{exposeAt("web", "", gentianov1alpha1.SurfaceGateway)}

	comp := func(name string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: tenant.NamespaceName(),
		}}
		c.Spec.ProfileRef.Name = name
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}
	// A service runs in no tenant's namespace and is in nobody's zone.
	svc := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "system-data"}}
	svc.Spec.ProfileRef.Name = "wiki"
	svc.Spec.Class = gentianov1alpha1.ComponentClassService

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(), wiki, unnamed, comp("wiki"), comp("notes"), svc,
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := zoneHostsOf(t, r, tenant.Name)
	want := []string{"notes", "wiki"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("hosts = %v, want %v: sorted, deduplicated, gateway entries only", got, want)
	}
}

// A tenant whose components declare no host still gets a projection. Its
// absence would be indistinguishable from "the operator has not looked yet",
// and the Composition would have no way to tell them apart.
func TestATenantWithNoComponentHostsStillGetsAProjection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	tenant := platformTenantFixture()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy()).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := zoneHostsOf(t, r, tenant.Name); len(got) != 0 {
		t.Fatalf("hosts = %v, want none", got)
	}
}

func exchangeProfilesOf(t *testing.T, r *TileProjectionReconciler, tenant string) []string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{
		Name: zoneHostsConfigMapName(tenant), Namespace: layout.Namespace(layout.Control),
	}
	if err := r.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("no projection for %s: %v", tenant, err)
	}
	if cm.Data[zoneExchangeProfilesKey] == "" {
		return nil
	}
	return strings.Split(cm.Data[zoneExchangeProfilesKey], "\n")
}

// The tenant's realm holds one scope per app that is handed an exchanged
// token, and the Composition makes them from this list. It follows the
// Components, as the routes that ask for the scopes do: an app is on it when
// an exposure of its profile says exchangeToken, whether a tenant installed
// it or the platform placed it, and an app that does not say so is not.
func TestExchangeProfilesFollowTheComponentsThatAsk(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tenant := platformTenantFixture()
	profile := func(name string, expose ...gentianov1alpha1.ExposureSpec) *gentianov1alpha1.ComponentProfile {
		p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
		p.Spec.Expose = expose
		return p
	}
	asking := func(name string) gentianov1alpha1.ExposureSpec {
		e := exposeAt(name, "", gentianov1alpha1.SurfaceGateway)
		e.ExchangeToken = true
		return e
	}
	comp := func(name, profileName, namespace string) *gentianov1alpha1.Component {
		c := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		c.Spec.ProfileRef.Name = profileName
		c.Spec.Class = gentianov1alpha1.ComponentClassApp
		return c
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		tenant.DeepCopy(),
		// One entry of several is enough: the scope is the app's.
		profile("wiki", exposeAt("web", "wiki", gentianov1alpha1.SurfaceGateway), asking("api")),
		profile("notes", asking("web")),
		// Does not ask, and so has no scope: its backend goes on reading
		// the identity headers.
		profile("plain", exposeAt("web", "", gentianov1alpha1.SurfaceGateway)),
		// Asks, and no component of this tenant is of it.
		profile("elsewhere", asking("web")),
		comp("wiki", "wiki", tenant.NamespaceName()),
		// Named after the profile, not the component: the route builder
		// names the scope AppTokenScope(profile), and two components of one
		// profile are one audience.
		comp("notes-a", "notes", tenant.NamespaceName()),
		comp("notes-b", "notes", tenant.NamespaceName()),
		comp("plain", "plain", tenant.NamespaceName()),
		// A component in nobody's namespace is in nobody's realm.
		comp("elsewhere", "elsewhere", "system-data"),
	).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "c1"}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := exchangeProfilesOf(t, r, tenant.Name)
	want := []string{"notes", "wiki"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("exchange profiles = %v, want %v: sorted, one per profile, only the ones that ask", got, want)
	}

	// The profile stops asking, and the next projection says so: the list
	// is what takes the scope out of the realm's exchange client.
	wiki := &gentianov1alpha1.ComponentProfile{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "wiki"}, wiki); err != nil {
		t.Fatal(err)
	}
	for i := range wiki.Spec.Expose {
		wiki.Spec.Expose[i].ExchangeToken = false
	}
	if err := c.Update(context.Background(), wiki); err != nil {
		t.Fatal(err)
	}
	if err := r.projectZoneHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := exchangeProfilesOf(t, r, tenant.Name); len(got) != 1 || got[0] != "notes" {
		t.Fatalf("exchange profiles = %v after wiki stopped asking, want notes alone", got)
	}
}
