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
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An app installed for everyone.
//
// spec.apps[].defaultGrant is part of the install as git declares it: every
// member of the tenant has access to this app by default. Two things follow
// from it, and they last differently. The app's group is marked, so that a
// person invited later has the app pre-selected -- that stays for as long as
// the group does. And the people who are members now are added to the group
// -- that is done ONCE, and Status.DefaultGrantedApps is where it is noted.
//
// Once, because membership of the group is the administrator's to change
// afterwards. A reconciler that added everybody on every pass would put back
// each person an administrator took out, a few seconds after they did.
//
// The note is dropped when the app is uninstalled or stops declaring the
// grant, so installing it again, or declaring the grant again, applies it
// again: once per install, not once per name.

const (
	// conditionDefaultGrantsApplied says whether every app declaring
	// defaultGrant has been granted. Absent when no app declares it.
	conditionDefaultGrantsApplied = "DefaultGrantsApplied"

	reasonDefaultGrantsApplied     = "Applied"
	reasonDefaultGrantWaiting      = "WaitingForAppGroup"
	reasonDefaultGrantFailed       = "GrantFailed"
	reasonDefaultGrantNoMeans      = "LifecycleServiceDisabled"
	defaultGrantRetryAfter         = 30 * time.Second
	defaultGrantMessageAppsAtMost  = 8
	defaultGrantMessageErrorAtMost = 240
)

// AppDefaultGranter adds a tenant's current members to an app's group and
// marks the group as granted by default. granted is false with no error
// while the app's group does not exist yet.
type AppDefaultGranter func(ctx context.Context, tenant, profile string) (granted bool, err error)

// applyDefaultGrants grants each app that declares defaultGrant and has not
// been granted yet, notes the ones done on the status -- which the caller
// writes -- and reports whether any is still outstanding and worth another
// reconcile.
//
// Nothing here returns an error. An app that could not be granted is a
// condition with a reason and a retry: who may open an app must not hold the
// tenant's readiness, and one app's failure must not keep another's grant
// from being applied.
func (r *TenantReconciler) applyDefaultGrants(ctx context.Context, tenant *gentianov1alpha1.Tenant) (retry bool) {
	logger := log.FromContext(ctx).WithName("default-grant").WithValues("tenant", tenant.Name)

	declared := map[string]bool{}
	var wanted []string
	for _, app := range tenant.Spec.Apps {
		if app.DefaultGrant && app.Profile != "" && !declared[app.Profile] {
			declared[app.Profile] = true
			wanted = append(wanted, app.Profile)
		}
	}

	// Keep the note only for what still declares the grant.
	done := map[string]bool{}
	var granted []string
	for _, profile := range tenant.Status.DefaultGrantedApps {
		if declared[profile] && !done[profile] {
			done[profile] = true
			granted = append(granted, profile)
		}
	}

	var waiting, failed []string
	lastErr := ""
	for _, profile := range wanted {
		if done[profile] {
			continue
		}
		if r.DefaultGrant == nil {
			failed = append(failed, profile)
			continue
		}
		ok, err := r.DefaultGrant(ctx, tenant.Name, profile)
		switch {
		case err != nil:
			logger.Error(err, "granting an app to the tenant's members (will retry)", "app", profile)
			failed = append(failed, profile)
			lastErr = err.Error()
		case !ok:
			waiting = append(waiting, profile)
		default:
			logger.Info("granted an app to the tenant's members", "app", profile)
			done[profile] = true
			granted = append(granted, profile)
		}
	}
	tenant.Status.DefaultGrantedApps = granted

	switch {
	case len(wanted) == 0:
		apimeta.RemoveStatusCondition(&tenant.Status.Conditions, conditionDefaultGrantsApplied)
		return false
	case len(failed) > 0 && r.DefaultGrant == nil:
		r.setCondition(tenant, conditionDefaultGrantsApplied, metav1.ConditionFalse, reasonDefaultGrantNoMeans,
			"the operator runs without its app-lifecycle service, so nobody was added to: "+appList(failed))
		// Nothing a later reconcile could change: the service is switched
		// on where the operator is started.
		return false
	case len(failed) > 0:
		if len(lastErr) > defaultGrantMessageErrorAtMost {
			lastErr = lastErr[:defaultGrantMessageErrorAtMost] + "…"
		}
		r.setCondition(tenant, conditionDefaultGrantsApplied, metav1.ConditionFalse, reasonDefaultGrantFailed,
			fmt.Sprintf("not granted yet, will be tried again: %s (%s)", appList(failed), lastErr))
		return true
	case len(waiting) > 0:
		r.setCondition(tenant, conditionDefaultGrantsApplied, metav1.ConditionFalse, reasonDefaultGrantWaiting,
			"waiting for the group of: "+appList(waiting))
		return true
	}
	r.setCondition(tenant, conditionDefaultGrantsApplied, metav1.ConditionTrue, reasonDefaultGrantsApplied,
		"every app installed for everyone has been granted to the tenant's members")
	return false
}

// appList names apps in a condition message, without letting a long list
// make the message one.
func appList(apps []string) string {
	if len(apps) > defaultGrantMessageAppsAtMost {
		return fmt.Sprintf("%s and %d more",
			strings.Join(apps[:defaultGrantMessageAppsAtMost], ", "), len(apps)-defaultGrantMessageAppsAtMost)
	}
	return strings.Join(apps, ", ")
}
