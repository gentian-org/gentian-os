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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

func tileFixtureRoute(name, namespace, host string) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(host)},
		},
	}
}

// project runs the reconciler over the given objects and reads back what it
// wrote, parsed the way the director parses it.
func project(t *testing.T, objs ...client.Object) []tilecatalogue.Tile {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "demo-cluster", KernelRealm: "kernel"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: tilecatalogue.ConfigMapName, Namespace: layout.Namespace(layout.Control)}
	if err := c.Get(context.Background(), key, cm); err != nil {
		t.Fatal(err)
	}
	catalogue, err := tilecatalogue.Parse([]byte(cm.Data[tilecatalogue.Key]))
	if err != nil {
		t.Fatal(err)
	}
	return catalogue.Tiles
}

func tileNames(got []tilecatalogue.Tile) []string {
	out := make([]string, 0, len(got))
	for _, t := range got {
		out = append(out, t.Name)
	}
	return out
}

// The catalogue is what the cluster routes and nothing else. A console whose
// route this operator did not compose is not offered, because a tile is a
// promise that the address answers.
func TestTheKernelCatalogueFollowsTheRoutes(t *testing.T) {
	headlamp := tileFixtureRoute(kernelRouteHeadlamp, servicesNamespace, "headlamp.k.example")
	argocd := tileFixtureRoute(kernelRouteArgoCD, servicesNamespace, "argocd.k.example")
	identity := tileFixtureRoute(kernelRouteKeycloakAdmin, servicesNamespace, "id.k.example")

	cases := map[string]struct {
		routes []client.Object
		want   string
	}{
		"every console routed": {
			routes: []client.Object{headlamp, argocd, identity},
			want:   "[headlamp argocd keycloak]",
		},
		"no kernel zone, so no console is routed": {
			routes: nil,
			want:   "[]",
		},
		"the layout has no observability namespace, so headlamp is not routed": {
			routes: []client.Object{argocd, identity},
			want:   "[argocd keycloak]",
		},
	}
	for name, c := range cases {
		if got := fmt.Sprint(tileNames(project(t, c.routes...))); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// The hosts, paths, icons and relations the kernel consoles have always had.
// A tile is a link a person follows, so each of these is load-bearing: the
// identity console's path names the realm because /admin/ alone lands on the
// master realm's console, which a kernel-realm administrator may not open, and
// Argo CD's path skips a login form the caller has no need of.
func TestTheKernelConsolesKeepTheirAddresses(t *testing.T) {
	got := project(t,
		tileFixtureRoute(kernelRouteHeadlamp, servicesNamespace, "headlamp.k.example"),
		tileFixtureRoute(kernelRouteArgoCD, servicesNamespace, "argocd.k.example"),
		tileFixtureRoute(kernelRouteKeycloakAdmin, servicesNamespace, "id.k.example"),
	)
	want := map[string]struct {
		url   string
		icon  string
		anyOf string
	}{
		"headlamp": {"https://headlamp.k.example/", "cluster", "[can_configure can_operate_system can_audit]"},
		"argocd":   {"https://argocd.k.example/auth/login", "sync", "[can_configure can_operate_system can_audit]"},
		"keycloak": {"https://id.k.example/auth/admin/kernel/console/", "identity", "[can_configure]"},
	}
	for _, tile := range got {
		w, ok := want[tile.Name]
		if !ok {
			t.Errorf("unexpected tile %s", tile.Name)
			continue
		}
		if tile.URL != w.url || tile.Icon != w.icon || fmt.Sprint(tile.AnyOf) != w.anyOf {
			t.Errorf("%s: url %s icon %s anyOf %v", tile.Name, tile.URL, tile.Icon, tile.AnyOf)
		}
		if tile.Object != "cluster:demo-cluster" {
			t.Errorf("%s: object %s, want the cluster", tile.Name, tile.Object)
		}
		if tile.DisplayName == "" || tile.Description == "" {
			t.Errorf("%s: the portal shows a name and a sentence", tile.Name)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", tileNames(got))
	}
}

func tileFixtureTenant(name string) *gentianov1alpha1.Tenant {
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = name
	return tenant
}

func tileFixtureComponent(name, namespace, profile string) *gentianov1alpha1.Component {
	comp := &gentianov1alpha1.Component{}
	comp.Name = name
	comp.Namespace = namespace
	comp.Spec.ProfileRef = gentianov1alpha1.ProfileRef{Name: profile}
	return comp
}

func tileFixtureProfile(name string, tile *gentianov1alpha1.ExposureTile) *gentianov1alpha1.ComponentProfile {
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Name = name
	profile.Spec.Expose = []gentianov1alpha1.ExposureSpec{{
		Name:     "web",
		Surface:  gentianov1alpha1.SurfaceGateway,
		AuthMode: gentianov1alpha1.AuthModeOIDC,
		Backend:  gentianov1alpha1.BackendRef{Service: "notes", Port: 8080},
		Tile:     tile,
	}}
	return profile
}

// An installed app appears on the portal because its profile says how, and
// because the operator routed it. Neither alone is enough: a declared tile
// with no route would be a link to nothing, and a route whose profile declares
// no tile is an entry point that was never meant to be advertised.
func TestAComponentTileNeedsBothTheProfileAndTheRoute(t *testing.T) {
	tile := &gentianov1alpha1.ExposureTile{
		DisplayName: "Notes", Description: "Write things down.", Icon: "notes",
		Path: "/dashboard", Relation: "can_launch",
	}
	tenant := tileFixtureTenant("demo")
	comp := tileFixtureComponent("notes", "tenant-demo", "notes")
	route := tileFixtureRoute("notes-web", "tenant-demo", "notes.demo.k.example")

	cases := map[string]struct {
		objs []client.Object
		want string
	}{
		"declared and routed": {
			objs: []client.Object{tenant, comp, tileFixtureProfile("notes", tile), route},
			want: "[demo/notes/web]",
		},
		"declared but not routed yet": {
			objs: []client.Object{tenant, comp, tileFixtureProfile("notes", tile)},
			want: "[]",
		},
		"routed but declares no tile": {
			objs: []client.Object{tenant, comp, tileFixtureProfile("notes", nil), route},
			want: "[]",
		},
		"routed in a namespace that is no tenant's": {
			objs: []client.Object{comp, tileFixtureProfile("notes", tile), route},
			want: "[]",
		},
	}
	for name, c := range cases {
		if got := fmt.Sprint(tileNames(project(t, c.objs...))); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}

	// And what it says, once it is there: the host comes from the route, the
	// path from the profile, and the relation is asked about the app object
	// rather than about the cluster.
	got := project(t, tenant, comp, tileFixtureProfile("notes", tile), route)
	if len(got) != 1 {
		t.Fatalf("got %v", tileNames(got))
	}
	if got[0].URL != "https://notes.demo.k.example/dashboard" || got[0].Object != "app:demo/notes" ||
		fmt.Sprint(got[0].AnyOf) != "[can_launch]" || got[0].DisplayName != "Notes" {
		t.Fatalf("tile = %+v", got[0])
	}
}

// Rebuilding a catalogue that has not changed must not rewrite the ConfigMap:
// everything mounting it is woken by a write, and a projection that churns on
// its requeue floor would wake them every ten minutes for nothing.
func TestAnUnchangedCatalogueIsNotRewritten(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tileFixtureRoute(kernelRouteArgoCD, servicesNamespace, "argocd.k.example")).Build()
	r := &TileProjectionReconciler{Client: c, Cluster: "demo-cluster", KernelRealm: "kernel"}
	ctx := context.Background()
	key := types.NamespacedName{Name: tilecatalogue.ConfigMapName, Namespace: layout.Namespace(layout.Control)}
	versions := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, key, cm); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, cm.ResourceVersion)
	}
	if versions[0] != versions[1] {
		t.Fatalf("the ConfigMap was rewritten: %v", versions)
	}
}
