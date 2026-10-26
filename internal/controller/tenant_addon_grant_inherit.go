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

	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Addons installed before an addon was granted on its own.
//
// Each addon is entitled by its own group, and an install now grants it
// (spec.apps[].addonGrants, or the App Store putting the chosen people in
// it). Addons installed before that got a group and nobody in it, so their
// tiles showed to no one. Once per tenant, every addon whose group has
// nobody in it is given its app's access: everyone when the app is for
// everyone, the app's group's members otherwise. Status.AddonGrantsInherited
// notes that it was done.
//
// A migration: remove it, the field and InheritAddonGrant once every tenant
// carries the note.

// AddonGrantInheritor gives an addon its app's access when nobody has the
// addon yet. done is false with no error while the addon's group does not
// exist.
type AddonGrantInheritor func(ctx context.Context, tenant, app, addon string, everyone bool) (done bool, err error)

// inheritAddonGrants runs the migration for a tenant that has not had it,
// sets the note on the status -- which the caller writes -- when every addon
// was handled, and reports whether it is worth another reconcile.
//
// An addon whose group is not there yet is one installed now, and is granted
// as that install says; it is not waited for.
func (r *TenantReconciler) inheritAddonGrants(ctx context.Context, tenant *gentianov1alpha1.Tenant) (retry bool) {
	if tenant.Status.AddonGrantsInherited || r.InheritAddonGrant == nil {
		return false
	}
	logger := log.FromContext(ctx).WithName("addon-grant").WithValues("tenant", tenant.Name)
	failed := false
	for _, app := range tenant.Spec.Apps {
		for _, addon := range app.Addons {
			if app.Profile == "" || addon == "" || app.AddonDefaultGranted(addon) {
				// Declared for everyone: the default grant applies it.
				continue
			}
			done, err := r.InheritAddonGrant(ctx, tenant.Name, app.Profile, addon, app.DefaultGrant)
			switch {
			case err != nil:
				logger.Error(err, "giving an addon its app's access (will retry)", "app", app.Profile, "addon", addon)
				failed = true
			case done:
				logger.Info("an addon has its app's access", "app", app.Profile, "addon", addon)
			}
		}
	}
	if failed {
		return true
	}
	tenant.Status.AddonGrantsInherited = true
	return false
}
