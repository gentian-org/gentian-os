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
	"os"
	"regexp"
	"sort"
	"strings"
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
	kcadmin "github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/envvar"
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
// What it does NOT own: membership, which arrives as signed Keycloak events
// and is applied by the operator's membership listener
// (membership_listener.go).
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

	// Admins answers whether an account is an administrator of a tenant, as
	// the store holds it (authz.TenantAdministrator). Nil asks Graph.
	Admins func(ctx context.Context, tenant, sub string) (bool, error)

	// notInEffectWarned holds the role holders already reported as not in
	// effect, so each is one log line and not one every requeue.
	notInEffectWarned map[string]bool

	// Roles is the arbiter's platform path, which the apps that are handed
	// their people's roles are told them through after each pass. Nil hands
	// nothing, which is what a cluster without app authorization runs.
	Roles RoleHandOver

	// BootstrapAdministrator answers the realm and the id of the account the
	// install made, "" when it is not there. Nil asks the kernel realm.
	BootstrapAdministrator func(ctx context.Context) (realm, id string, err error)

	// RightsTaken is told, by tenant, the people a change of what a position
	// holds took something from (TenantReconciler.RightsTaken): their
	// sessions end, so no token carries what they no longer hold. Nil ends
	// nothing.
	RightsTaken func(ctx context.Context, tenant string, subjects []string) error
}

// +kubebuilder:rbac:groups=gentianos.io,resources=clusters,verbs=get;list;watch

// Reconcile projects the cluster's roles and its tenants into the graph.
func (r *AuthzProjectionReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if r.Graph == nil || r.Cluster == "" {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)

	holders, nextExpiry, err := r.roleHolders(ctx, time.Now())
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read role holders from the Cluster claim: %w", err)
	}
	holders, err = r.withBootstrapAdministrator(ctx, holders)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("find the install's administrator: %w", err)
	}
	if err := r.Graph.ReconcileClusterRoles(ctx, r.Cluster, holders); err != nil {
		return ctrl.Result{}, fmt.Errorf("project cluster roles: %w", err)
	}

	tenants, err := r.tenantRights(ctx, holders)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list tenants: %w", err)
	}
	if err := r.Graph.ReconcileTenants(ctx, r.Cluster, tenants); err != nil {
		return ctrl.Result{}, fmt.Errorf("project tenants: %w", err)
	}

	// What each tenant has installed, so that an app's tile and its route can
	// be asked about, and the positions each tenant has: the four every
	// tenant has, and the ones it declares.
	apps, positions, err := r.declared(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list installed apps and positions: %w", err)
	}
	if err := r.Graph.ReconcileApps(ctx, apps); err != nil {
		return ctrl.Result{}, fmt.Errorf("project installed apps: %w", err)
	}
	// A tenant whose declared positions the store cannot hold is left as it
	// is and the others are written; the error names it and asks again.
	taken, positionsErr := r.Graph.ReconcilePositionsTaking(ctx, positions, stillInstalled(apps))
	r.endSessionsOfTaken(ctx, taken)
	// What follows from what is given, written for every tenant here, the
	// one whose positions were refused included: what it has stays derived
	// from what the store holds for it.
	names := make([]string, 0, len(positions))
	for tenant := range positions {
		names = append(names, tenant)
	}
	sort.Strings(names)
	if err := authz.ReconcileDerived(ctx, r.Graph, names); err != nil {
		return ctrl.Result{}, fmt.Errorf("derive what follows from what is given: %w", err)
	}
	if positionsErr != nil {
		return ctrl.Result{}, fmt.Errorf("project positions: %w", positionsErr)
	}
	if r.Roles != nil {
		if err := handOverRoles(ctx, r.Client, r.Graph, r.Roles); err != nil {
			return ctrl.Result{}, err
		}
	}

	logger.V(1).Info("authorization structure projected",
		"cluster", r.Cluster, "holders", len(holders), "tenants", len(tenants))
	// Again when the next role holder's entry expires, so the role goes at
	// its time and not up to a requeue later.
	requeue := authzProjectionRequeue
	if !nextExpiry.IsZero() {
		if until := time.Until(nextExpiry) + time.Second; until < requeue {
			requeue = until
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// endSessionsOfTaken ends the sessions of the people a change of positions
// took something from. What was taken is gone from the store already and a
// later pass would not find it again, so a failure is said and not retried.
func (r *AuthzProjectionReconciler) endSessionsOfTaken(ctx context.Context, taken map[string][]string) {
	if r.RightsTaken == nil {
		return
	}
	tenants := make([]string, 0, len(taken))
	for tenant := range taken {
		tenants = append(tenants, tenant)
	}
	sort.Strings(tenants)
	for _, tenant := range tenants {
		log.FromContext(ctx).Info("a change of positions took something from people; their sessions end",
			"tenant", tenant, "people", len(taken[tenant]))
		if err := r.RightsTaken(ctx, tenant, taken[tenant]); err != nil {
			log.FromContext(ctx).Error(err, "the sessions of people a change of positions took something from were not all ended",
				"tenant", tenant)
		}
	}
}

// stillInstalled says whether a thing a position holds a role on is still
// there: an installed app or add-on, or a role one of them declares.
func stillInstalled(apps map[string][]authz.InstalledApp) func(object string) bool {
	there := map[string]bool{}
	var add func(tenant string, a authz.InstalledApp)
	add = func(tenant string, a authz.InstalledApp) {
		there[authz.App(tenant, a.Profile)] = true
		for _, role := range a.Roles {
			there[authz.Role(tenant, a.Profile, role.Name)] = true
		}
		for _, addon := range a.Addons {
			add(tenant, addon)
		}
	}
	for tenant, installed := range apps {
		for _, a := range installed {
			add(tenant, a)
		}
	}
	return func(object string) bool { return there[object] }
}

// withBootstrapAdministrator adds the account the install made as the
// cluster's administrator, while the Cluster claim names none.
//
// Somebody has to administer a cluster before anybody can be named on its
// claim through the console, and a claim that names nobody must not leave
// the cluster to nobody. The install makes one account in the kernel realm,
// admin@<kernel domain>; it holds admin until the claim names an
// administrator, and from then on only if the claim names it. An account
// that is not there yet, or no longer, holds nothing, and is said.
func (r *AuthzProjectionReconciler) withBootstrapAdministrator(ctx context.Context, holders []authz.RoleHolder) ([]authz.RoleHolder, error) {
	for _, h := range holders {
		if h.Relation == "admin" && h.Tenant == "" {
			return holders, nil
		}
	}
	lookup := r.BootstrapAdministrator
	if lookup == nil {
		lookup = r.bootstrapAdministrator
	}
	realm, id, err := lookup(ctx)
	if err != nil {
		return nil, err
	}
	if id == "" {
		log.FromContext(ctx).Info("the Cluster claim names no administrator and the install's account is not there, so nobody administers this cluster",
			"setting", "spec.platformAccess.roleHolders")
		return holders, nil
	}
	return append(holders, authz.RoleHolder{Relation: "admin", Realm: realm, Subject: id}), nil
}

// bootstrapAdministrator asks the kernel realm for the account the install
// made, by its name.
func (r *AuthzProjectionReconciler) bootstrapAdministrator(ctx context.Context) (realm, id string, err error) {
	domain := os.Getenv("KERNEL_DOMAIN")
	if domain == "" {
		return "", "", nil
	}
	realm = envvar.Or("GENTIAN_PLATFORM_REALM", "kernel")
	kc, err := kcadmin.LoadKeycloakAdminClient(ctx, r.Client)
	if err != nil {
		return "", "", err
	}
	id, err = kc.UserIDByUsername(ctx, realm, "admin@"+domain)
	return realm, id, err
}

// roleHolders is spec.platformAccess.roleHolders from the Cluster claim: the
// roles the cluster's administrator gave people by name, each the account of
// its tenant's realm. An entry grants nothing when its role is not one a
// holder may hold, when the people of its tenant may not hold that role or
// its stated scope is not one the role has for them (authz.EntryScope), when its tenant is not one this cluster has with a
// realm of its own -- the platform tenant's being the kernel realm -- or
// when its subject is not an account id: the claim's schema refuses those
// shapes, and a claim written past it is not widened by guessing.
//
// An entry whose expiresAt has passed grants nothing either, and nor does one
// whose person of a tenant other than the platform's is no longer an
// administrator of it: the entry stays in the claim, which is the record,
// and its role holds again once they are. next is the earliest expiry still
// ahead, zero when none is.
func (r *AuthzProjectionReconciler) roleHolders(ctx context.Context, now time.Time) (out []authz.RoleHolder, next time.Time, err error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: clusterClaimGVK.Group, Version: clusterClaimGVK.Version, Kind: clusterClaimGVK.Kind + "List",
	})
	if err := r.List(ctx, list, client.InNamespace(clusterConfigNamespace)); err != nil {
		return nil, time.Time{}, err
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return nil, time.Time{}, err
	}
	// The realm a tenant's people sign in to: its own, named after it, or
	// for the platform tenant the kernel realm it adopts. A tenant that
	// adopts another realm has no people of its own to name.
	realms := map[string]string{}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		if t.DeletionTimestamp != nil {
			continue
		}
		if realm := tenantRealmName(t); realm == t.Name || t.Name == authz.PlatformTenant {
			realms[t.Name] = realm
		}
	}
	for i := range list.Items {
		entries, _, _ := unstructured.NestedSlice(list.Items[i].Object, "spec", "platformAccess", "roleHolders")
		for _, e := range entries {
			m, _ := e.(map[string]interface{})
			role, _ := m["role"].(string)
			tenant, _ := m["tenant"].(string)
			subject, _ := m["subject"].(string)
			stated, _ := m["scope"].(string)
			expires, _ := m["expiresAt"].(string)
			rel, ok := authz.HolderRoles[role]
			scope, allowed := authz.EntryScope(role, tenant, stated)
			realm, known := realms[tenant]
			if !ok || !allowed || !known || !holderSubject.MatchString(subject) {
				log.FromContext(ctx).Info("a role holder on the Cluster claim is not projected",
					"role", role, "scope", stated, "tenant", tenant, "subject", subject)
				continue
			}
			if expires != "" {
				at, err := time.Parse(time.RFC3339, expires)
				if err != nil || !at.After(now) {
					log.FromContext(ctx).V(1).Info("a role holder's entry has expired or names no time; not projected",
						"role", role, "tenant", tenant, "subject", subject, "expiresAt", expires)
					continue
				}
				if next.IsZero() || at.Before(next) {
					next = at
				}
			}
			if tenant != authz.PlatformTenant {
				admin, err := r.isAdmin(ctx, tenant, subject)
				if err != nil {
					return nil, time.Time{}, fmt.Errorf("ask whether %s administers %s: %w", subject, tenant, err)
				}
				key := role + "/" + tenant + "/" + subject
				if !admin {
					if r.notInEffectWarned == nil {
						r.notInEffectWarned = map[string]bool{}
					}
					if !r.notInEffectWarned[key] {
						r.notInEffectWarned[key] = true
						log.FromContext(ctx).Info("a role holder on the Cluster claim is not in effect: no longer an administrator of the tenant",
							"role", role, "tenant", tenant, "subject", subject)
					}
					continue
				}
				delete(r.notInEffectWarned, key)
			}
			h := authz.RoleHolder{Relation: rel, Realm: realm, Subject: subject}
			if scope != authz.ScopeCluster {
				h.Tenant = strings.TrimPrefix(scope, "tenant:")
			}
			out = append(out, h)
		}
	}
	return out, next, nil
}

func (r *AuthzProjectionReconciler) isAdmin(ctx context.Context, tenant, sub string) (bool, error) {
	if r.Admins != nil {
		return r.Admins(ctx, tenant, sub)
	}
	return authz.TenantAdministrator(ctx, r.Graph, "", tenant, sub)
}

// holderSubject is what a holder's subject may look like: Keycloak's user
// ids are UUIDs.
var holderSubject = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// declared is what every tenant has installed and which positions it has.
// A tenant being deleted is listed with nothing, so its apps and positions
// leave the store with it.
func (r *AuthzProjectionReconciler) declared(ctx context.Context) (map[string][]authz.InstalledApp, map[string][]authz.PositionSpec, error) {
	list := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, list); err != nil {
		return nil, nil, err
	}
	apps := map[string][]authz.InstalledApp{}
	positions := map[string][]authz.PositionSpec{}
	for i := range list.Items {
		tenant := &list.Items[i]
		apps[tenant.Name], positions[tenant.Name] = nil, nil
		if tenant.DeletionTimestamp != nil {
			continue
		}
		installed, err := installedAppsOf(ctx, r.Client, tenant)
		if err != nil {
			return nil, nil, err
		}
		apps[tenant.Name] = installed
		positions[tenant.Name] = positionsOf(tenant, installed)
	}
	return apps, positions, nil
}

// tenantRights is every tenant this cluster has, excluding those being
// deleted: a tenant on its way out should lose its attachment before its
// namespace goes, not after. With each, the role holders the claim names its
// perimeter approvers and its auditors.
func (r *AuthzProjectionReconciler) tenantRights(ctx context.Context, holders []authz.RoleHolder) ([]authz.TenantRights, error) {
	list := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	approvers, auditors := map[string][]string{}, map[string][]string{}
	for _, h := range holders {
		if h.Tenant == "" {
			continue
		}
		user, err := authz.HolderUser(h.Realm, h.Subject)
		if err != nil {
			continue
		}
		switch h.Relation {
		case "perimeter_approver":
			approvers[h.Tenant] = append(approvers[h.Tenant], user)
		case "auditor":
			auditors[h.Tenant] = append(auditors[h.Tenant], user)
		}
	}
	out := make([]authz.TenantRights, 0, len(list.Items))
	for i := range list.Items {
		tenant := &list.Items[i]
		if tenant.DeletionTimestamp != nil {
			continue
		}
		out = append(out, authz.TenantRights{Name: tenant.Name,
			PerimeterApprovers: approvers[tenant.Name], Auditors: auditors[tenant.Name]})
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
