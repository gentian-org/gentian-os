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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

// The tile catalogue is cluster state, so the operator projects it.
//
// It used to be a list compiled into the director: three kernel UIs in a YAML
// file embedded in the binary, each with the hostname it answers on and the
// relations that open it. Every one of those facts is already here. This
// operator writes the HTTPRoute that makes headlamp.<kernel> answer, and it
// reads the ComponentProfile of every installed app. A second copy in the
// director could only drift from the first, and while it existed an installed
// app could not appear on the portal without a director release.
//
// So the catalogue is projected from what is actually routed, and the director
// is left with the question it exists to answer: which of these may this
// caller open. A tile whose route does not exist is not in the catalogue,
// which means a console the cluster does not serve yet cannot be advertised as
// a broken link.
//
// The projection is one ConfigMap in the control namespace, beside the
// director, and it is declarative like the rest: an app uninstalled loses its
// tile because the projection is rebuilt, not because anything remembers to
// delete it.

const (
	// tileProjectionRequeue is the floor under which the catalogue is rebuilt
	// even when nothing watched has changed. Hostnames come from routes and
	// relations from profiles, both of which are watched; the floor is for the
	// ConfigMap itself, which a person can edit and which nothing else
	// restores.
	tileProjectionRequeue = 10 * time.Minute
)

// TileProjectionReconciler keeps the projected catalogue equal to what the
// cluster routes.
type TileProjectionReconciler struct {
	client.Client
	// Cluster is the id this cluster is known by: the object a kernel
	// console's relations are held on.
	Cluster string
	// KernelRealm is the realm the platform's own accounts live in. Keycloak's
	// administration console is per realm, so the tile has to name it.
	KernelRealm string
}

// kernelTile is one of the kernel's own UIs, keyed by the HTTPRoute that makes
// it reachable.
type kernelTile struct {
	// route is the name of the HTTPRoute this operator composes for the
	// console. The route is what decides whether the tile exists at all:
	// kernelHTTPRouteSpecs leaves it out on a cluster with no kernel zone, and
	// leaves headlamp out where the layout has no observability namespace.
	route string
	name  string
	// displayName and description are the portal's words for it, in the
	// language of the person using the cluster rather than of the chart.
	displayName string
	description string
	icon        string
	// path is where within the host the console is entered. Empty is the
	// front page.
	path string
	// anyOf are the cluster relations that open it. Any one is enough.
	anyOf []string
}

// kernelTileTable is the kernel's own UIs, in the order the portal shows them.
//
// The hostname of each is deliberately absent: it is read from the route, so
// the tile and the thing it points at cannot disagree. What is here is only
// what a route does not say, which is what a person should be told about the
// console and who may open it.
func kernelTileTable(kernelRealm string) []kernelTile {
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}
	return []kernelTile{
		{
			route:       kernelRouteHeadlamp,
			name:        "headlamp",
			displayName: "Cluster",
			description: "The cluster as Kubernetes sees it — nodes, workloads, events — with your own identity.",
			icon:        "cluster",
			anyOf:       []string{"can_configure", "can_operate_system", "can_audit"},
		},
		{
			route:       kernelRouteArgoCD,
			name:        "argocd",
			displayName: "Deployments",
			description: "What git says the cluster should run, and whether it does.",
			icon:        "sync",
			// The sign-in entry, not the front page. Argo CD's front page is a
			// login form with a button that starts the realm's flow; the
			// person following this tile has a session already, so the tile
			// skips the form and the flow completes without them typing
			// anything.
			path:  "/auth/login",
			anyOf: []string{"can_configure", "can_operate_system", "can_audit"},
		},
		{
			// The same hostname that issues the tokens: Keycloak refuses its
			// own Admin REST API when the console is served on a second one,
			// so there is no id-admin.<kernel> (networking.md §3). That is why
			// this tile follows the kernel-id-admin route rather than a route
			// of its own name.
			route:       kernelRouteKeycloakAdmin,
			name:        "keycloak",
			displayName: "Identity",
			description: "Realms, clients and the people in them.",
			icon:        "identity",
			// Keycloak is served under /auth, and its administration console
			// is per realm: a link to /admin/ alone lands on the master
			// realm's console, which a kernel-realm administrator may not
			// open. That refusal reads like a broken sign-in rather than like
			// a wrong address, so the realm is named here.
			path:  fmt.Sprintf("/auth/admin/%s/console/", kernelRealm),
			anyOf: []string{"can_configure"},
		},
	}
}

// The markers are a free-floating block: controller-gen ignores a block that
// is part of a declaration's doc comment.
//
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=components;componentprofiles;tenants,verbs=get;list;watch

// Reconcile rebuilds the catalogue and writes it.
func (r *TileProjectionReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if r.Cluster == "" {
		// Without a cluster id there is no object to hold a kernel console's
		// relations on, so every kernel tile would be unanswerable. Say so
		// rather than project a catalogue nobody can be checked against.
		logger.Info("no cluster id is configured, so no tile catalogue is projected",
			"setting", "GENTIAN_DEPLOYMENTS_CLUSTER_ID")
		return ctrl.Result{RequeueAfter: tileProjectionRequeue}, nil
	}

	catalogue := tilecatalogue.Catalogue{}
	kernel, err := r.kernelTiles(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("project the kernel consoles: %w", err)
	}
	catalogue.Tiles = append(catalogue.Tiles, kernel...)
	apps, err := r.componentTiles(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("project the component tiles: %w", err)
	}
	catalogue.Tiles = append(catalogue.Tiles, apps...)

	if err := r.write(ctx, catalogue); err != nil {
		return ctrl.Result{}, fmt.Errorf("write the tile catalogue: %w", err)
	}
	logger.V(1).Info("tile catalogue projected",
		"cluster", r.Cluster, "kernel", len(kernel), "components", len(apps))
	return ctrl.Result{RequeueAfter: tileProjectionRequeue}, nil
}

// kernelTiles is one tile per kernel console this cluster actually routes.
//
// The route is looked up by name: it is the same object this operator creates,
// so a console that is not served has no route, and a console whose hostname
// changed carries the new one without anything else being edited.
func (r *TileProjectionReconciler) kernelTiles(ctx context.Context) ([]tilecatalogue.Tile, error) {
	object := "cluster:" + r.Cluster
	var out []tilecatalogue.Tile
	for _, kt := range kernelTileTable(r.KernelRealm) {
		route := &gatewayv1.HTTPRoute{}
		err := r.Get(ctx, types.NamespacedName{Name: kt.route, Namespace: servicesNamespace}, route)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(route.Spec.Hostnames) == 0 {
			// A route matching every hostname serves no tile: there is no one
			// address to send a person to.
			continue
		}
		out = append(out, tilecatalogue.Tile{
			Name:        kt.name,
			DisplayName: kt.displayName,
			Description: kt.description,
			Icon:        kt.icon,
			URL:         tilecatalogue.URL(string(route.Spec.Hostnames[0]), kt.path),
			Object:      object,
			AnyOf:       kt.anyOf,
		})
	}
	return out, nil
}

// componentTiles is one tile per exposure of an installed component whose
// profile declares one.
//
// An exposure with no tile is reachable and unadvertised, which is what an API
// or a callback endpoint should be. An exposure with a tile still needs its
// route to exist: until the component's zone is ready nothing is routed, and a
// tile pointing at a hostname that answers nothing is worse than no tile.
func (r *TileProjectionReconciler) componentTiles(ctx context.Context) ([]tilecatalogue.Tile, error) {
	components := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, components); err != nil {
		return nil, err
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return nil, err
	}
	byNamespace := map[string]string{}
	for i := range tenants.Items {
		if tenants.Items[i].DeletionTimestamp != nil {
			continue
		}
		byNamespace[tenants.Items[i].NamespaceName()] = tenants.Items[i].Name
	}

	var out []tilecatalogue.Tile
	for i := range components.Items {
		comp := &components.Items[i]
		if comp.DeletionTimestamp != nil {
			continue
		}
		// A tile belongs on a tenant's page, and the object its relation is
		// held on is the tenant's install. A component that is not in a
		// tenant's namespace, a system service or a shared backend, is nobody's
		// tile even where it is routed.
		tenant, ok := byNamespace[comp.Namespace]
		if !ok {
			continue
		}
		profile := &gentianov1alpha1.ComponentProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: comp.Spec.ProfileRef.Name}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		for j := range profile.Spec.Expose {
			e := &profile.Spec.Expose[j]
			if e.Tile == nil || e.Surface != gentianov1alpha1.SurfaceGateway {
				continue
			}
			route := &gatewayv1.HTTPRoute{}
			err := r.Get(ctx, types.NamespacedName{Name: comp.Name + "-" + e.Name, Namespace: comp.Namespace}, route)
			if errors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if len(route.Spec.Hostnames) == 0 {
				continue
			}
			out = append(out, tilecatalogue.Tile{
				// Unique across the catalogue: two tenants may install the
				// same profile, and the portal has to tell the two tiles
				// apart.
				Name:        tenant + "/" + comp.Name + "/" + e.Name,
				DisplayName: e.Tile.DisplayName,
				Description: e.Tile.Description,
				Icon:        e.Tile.Icon,
				URL:         tilecatalogue.URL(string(route.Spec.Hostnames[0]), e.Tile.Path),
				// app:<tenant>/<profile>, the object an installed app's own
				// permissions hang off (authorization-model.md §4).
				Object: "app:" + tenant + "/" + comp.Spec.ProfileRef.Name,
				AnyOf:  []string{e.Tile.Relation},
			})
		}
	}
	// Stable order, so a projection that did not change does not rewrite the
	// ConfigMap and wake everything that watches it.
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// write puts the catalogue in the control namespace, creating the ConfigMap
// the first time and patching it only when the content differs.
func (r *TileProjectionReconciler) write(ctx context.Context, catalogue tilecatalogue.Catalogue) error {
	rendered, err := tilecatalogue.Marshal(catalogue)
	if err != nil {
		return err
	}
	key := types.NamespacedName{Name: tilecatalogue.ConfigMapName, Namespace: layout.Namespace(layout.Control)}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string]string{tilecatalogue.Key: rendered},
	}

	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, key, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Data, desired.Data) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = desired.Data
	return r.Patch(ctx, existing, patch)
}

// SetupWithManager rebuilds the catalogue when a component, a profile, a
// tenant or a route changes, and on the requeue floor otherwise.
//
// Routes are watched because they are the catalogue's source of hostnames: a
// console that becomes reachable should appear without waiting out the floor.
func (r *TileProjectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// One request for the whole projection: it is not per-object work, and a
	// fixed key collapses a burst of events into one pass.
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("tile-projection").
		For(&gentianov1alpha1.Component{}, builder.WithPredicates()).
		Watches(&gentianov1alpha1.ComponentProfile{}, one).
		Watches(&gentianov1alpha1.Tenant{}, one).
		Watches(&gatewayv1.HTTPRoute{}, one).
		Complete(r)
}
