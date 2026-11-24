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
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// appWaits is what each app of a tenant is still waiting for in the kernel:
// its realm client, its PostgreSQL role and database, its MariaDB, its
// bucket, its cache user, the tenant's Memcached. Keyed by the app's
// profile name; the values say what, for the Tenant's status.
//
// A kernel resource that is not ready yet holds back the app it is for, and
// only that app. The data-plane stage records it here and goes on instead of
// ending the reconcile, so the tenant's other apps are installed, moved and
// removed while one of them waits (ensureAppComponentsHolding). What every
// app needs -- the realm itself, its groups, its first administrator -- still
// holds back the whole tenant, as nothing of it works without them.
//
// A nil appWaits records nothing.
type appWaits map[string][]string

// add records that app waits for what.
func (w appWaits) add(app, what string) {
	if w == nil || app == "" {
		return
	}
	if !slices.Contains(w[app], what) {
		w[app] = append(w[app], what)
	}
}

// addAll records that every one of apps waits for what.
func (w appWaits) addAll(apps []string, what string) {
	for _, app := range apps {
		w.add(app, what)
	}
}

// holds reports whether app waits for anything.
func (w appWaits) holds(app string) bool {
	return len(w[app]) > 0
}

// describe is the waiting apps and what each waits for, in a stable order:
// "openproject-ce (the tenant's Memcached); vikunja-ce (its PostgreSQL role)".
func (w appWaits) describe() string {
	apps := make([]string, 0, len(w))
	for app := range w {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	parts := make([]string, 0, len(apps))
	for _, app := range apps {
		parts = append(parts, fmt.Sprintf("%s (%s)", app, strings.Join(w[app], ", ")))
	}
	return strings.Join(parts, "; ")
}

// installedComponentNames is the Components the tenant's spec.apps asks for:
// each app's and each of its add-ons'. Read from the entries alone, without
// the profiles, so it can be answered while a profile is missing.
func installedComponentNames(tenant *gentianov1alpha1.Tenant) (map[string]struct{}, bool) {
	names := map[string]struct{}{}
	for _, app := range tenant.Spec.Apps {
		name := app.Profile
		if name == "" && app.ProfileRef != nil {
			name = app.ProfileRef.Name
		}
		if name == "" {
			// An entry that names no profile cannot be told apart from
			// any Component: nothing is removed while it is there.
			return nil, false
		}
		names[name] = struct{}{}
		for _, addon := range app.Addons {
			if addon != "" {
				names[addon] = struct{}{}
			}
		}
	}
	return names, true
}

// pruneUninstalledComponents deletes the Component of every app whose install
// has left spec.apps.
//
// Uninstalling is a commit that removes the entry, and this is where the
// cluster follows it. It runs first in every reconcile of a live tenant and
// waits for nothing: not for a missing profile, not for the tenant's kernel
// resources, not for another app. An uninstall that waited behind those took
// as long as the slowest of them -- or forever, while one of them could not
// be made. ensureAppComponents removes them too; this is the same removal,
// made before anything can stop the reconcile.
//
// A failure is logged and left to the next pass: it must not be what stops
// the rest of the tenant either.
func (r *TenantReconciler) pruneUninstalledComponents(ctx context.Context, tenant *gentianov1alpha1.Tenant) {
	wanted, ok := installedComponentNames(tenant)
	if !ok {
		return
	}
	installed := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, installed, client.InNamespace(tenantNamespaceName(tenant)),
		client.MatchingLabels{componentOriginLabel: componentOriginInstall}); err != nil {
		log.FromContext(ctx).Error(err, "list the tenant's installed components; uninstalls wait for the next pass",
			"tenant", tenant.Name)
		return
	}
	for i := range installed.Items {
		comp := &installed.Items[i]
		if _, keep := wanted[comp.Name]; keep || comp.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, comp); client.IgnoreNotFound(err) != nil {
			log.FromContext(ctx).Error(err, "remove a component that is no longer installed; tried again on the next pass",
				"tenant", tenant.Name, "component", comp.Name)
			continue
		}
		log.FromContext(ctx).Info("removed a component that is no longer installed",
			"tenant", tenant.Name, "component", comp.Name)
	}
}
