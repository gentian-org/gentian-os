/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
