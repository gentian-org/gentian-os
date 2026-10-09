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
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// The authorization graph's STRUCTURE is cluster state, so the operator
// projects it.
//
// It used to be the director's: at start it created the OpenFGA store and
// model, projected the cluster's roles from the claim and its tenants from the
// tenant manifests, and it held a token that could write any of it. That was
// never the plan. The director's job is to read the graph to decide whether a
// caller may make a call, and to write git; Argo CD syncs git and the operator
// turns it into cluster state. Tuples describing which tenants a cluster has
// and which group holds which role over it are exactly that kind of state.
//
// So this reconciler owns them, and the director is left with read access.
//
// What it does NOT own, yet: membership, which arrives as signed Keycloak
// events and is still applied by the director, and session revocation, which
// belongs with the edge authorization service that reads it. Both are
// event-driven rather than projections of declared state, and both move on
// their own step.
//
// Declarative, like the functions it calls: a role whose group the claim no
// longer names loses its tuple, so authority over a cluster is taken away by
// editing the claim rather than by remembering to delete something.

const (
	// authzProjectionRequeue is the floor under which the graph is re-read
	// even when nothing watched has changed. The claim is not a CRD this
	// operator owns, so an edit to it may arrive without an event.
	authzProjectionRequeue = 10 * time.Minute
)

var clusterClaimGVK = schema.GroupVersionKind{
	Group:   gentianov1alpha1.GroupVersion.Group,
	Version: gentianov1alpha1.GroupVersion.Version,
	Kind:    "Cluster",
}

// AuthzProjectionReconciler keeps the graph's structure equal to the cluster's.
type AuthzProjectionReconciler struct {
	client.Client
	// Cluster is the id this cluster is known by, the object every cluster
	// relation hangs off.
	Cluster string
	// Graph is the OpenFGA client. Nil disables the reconciler entirely,
	// which is what a cluster with no authorization service runs.
	Graph *authz.OpenFGA
}

// +kubebuilder:rbac:groups=gentianos.io,resources=clusters,verbs=get;list;watch

// Reconcile projects the cluster's roles and its tenants into the graph.
func (r *AuthzProjectionReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if r.Graph == nil || r.Cluster == "" {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)

	roles, err := r.platformRoles(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read platform roles from the Cluster claim: %w", err)
	}
	if len(roles) == 0 {
		// Worth saying rather than passing over: an empty projection is a
		// cluster nobody administers, and it looks exactly like a broken
		// login from the outside.
		logger.Info("the Cluster claim assigns no platform roles, so nobody administers this cluster",
			"setting", "spec.platformRoles")
	}
	if err := r.Graph.ReconcileClusterRoles(ctx, r.Cluster, roles); err != nil {
		return ctrl.Result{}, fmt.Errorf("project cluster roles: %w", err)
	}

	tenants, err := r.tenantRights(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list tenants: %w", err)
	}
	if err := r.Graph.ReconcileTenants(ctx, r.Cluster, tenants); err != nil {
		return ctrl.Result{}, fmt.Errorf("project tenants: %w", err)
	}

	// What each tenant has installed, so that an app's tile and its route can
	// be asked about: the app attached to its tenant, and entitled by the
	// Keycloak group the identity reconciler creates for it.
	apps, err := r.installedApps(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list installed apps: %w", err)
	}
	if err := r.Graph.ReconcileApps(ctx, apps); err != nil {
		return ctrl.Result{}, fmt.Errorf("project installed apps: %w", err)
	}

	logger.V(1).Info("authorization structure projected",
		"cluster", r.Cluster, "roles", len(roles), "tenants", len(tenants))
	return ctrl.Result{RequeueAfter: authzProjectionRequeue}, nil
}

// platformRoles is spec.platformRoles from the Cluster claim: the group that
// holds each platform role.
//
// From the claim and not from a ConfigMap derived from it: the claim in git is
// where authority over a cluster is written down, and a cluster must not be
// able to widen its own administrators' rights by editing something inside
// itself.
func (r *AuthzProjectionReconciler) platformRoles(ctx context.Context) (map[string]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: clusterClaimGVK.Group, Version: clusterClaimGVK.Version, Kind: clusterClaimGVK.Kind + "List",
	})
	if err := r.List(ctx, list, client.InNamespace(clusterConfigNamespace)); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range list.Items {
		roles, found, err := unstructured.NestedStringMap(list.Items[i].Object, "spec", "platformRoles")
		if err != nil || !found {
			continue
		}
		for role, group := range roles {
			if role != "" && group != "" {
				out[role] = group
			}
		}
	}
	return out, nil
}

// installedApps is what every tenant has installed, by profile name, with the
// addons activated inside each. A tenant being deleted is listed with nothing,
// so its apps leave the graph with it.
//
// From spec.apps, which is the same list the identity reconciler creates the
// apps' groups from: an app has a group exactly when it has an object here.
// An entry whose profile cannot be named is skipped rather than failing the
// projection, because one unresolvable install must not stop every other
// tenant's rights from being written.
func (r *AuthzProjectionReconciler) installedApps(ctx context.Context) (map[string][]authz.InstalledApp, error) {
	list := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	out := map[string][]authz.InstalledApp{}
	for i := range list.Items {
		tenant := &list.Items[i]
		out[tenant.Name] = nil
		if tenant.DeletionTimestamp != nil {
			continue
		}
		for _, app := range tenant.Spec.Apps {
			profile, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
			if err != nil {
				log.FromContext(ctx).Info("installed app not projected: its profile cannot be named",
					"tenant", tenant.Name, "error", err.Error())
				continue
			}
			out[tenant.Name] = append(out[tenant.Name], authz.InstalledApp{Profile: profile, Addons: app.Addons})
		}
	}
	return out, nil
}

// tenantRights is every tenant this cluster has, excluding those being
// deleted: a tenant on its way out should lose its attachment before its
// namespace goes, not after. With each, what its manifest says about who
// approves its public addresses (spec.perimeter.adminsApprove).
func (r *AuthzProjectionReconciler) tenantRights(ctx context.Context) ([]authz.TenantRights, error) {
	list := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	out := make([]authz.TenantRights, 0, len(list.Items))
	for i := range list.Items {
		tenant := &list.Items[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		out = append(out, authz.TenantRights{
			Name:          tenant.Name,
			AdminsApprove: tenant.Spec.Perimeter != nil && tenant.Spec.Perimeter.AdminsApprove,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SetupWithManager runs the projection when a tenant or the claim changes, and
// on the requeue floor otherwise.
func (r *AuthzProjectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(clusterClaimGVK)
	// One request for the whole projection: it is not per-object work, and a
	// fixed key collapses a burst of tenant events into one pass.
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("authz-projection").
		For(&gentianov1alpha1.Tenant{}, builder.WithPredicates()).
		Watches(claim, one).
		Complete(r)
}
