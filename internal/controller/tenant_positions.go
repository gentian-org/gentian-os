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

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/placement"
)

// What a tenant declares that the rights store is written from: its
// installed apps and its positions. One reading of it, for the projection
// that writes the store and for the backup that tells what the store holds
// beyond it.

// installedAppsOf is what a tenant has installed, by profile name, each with
// the add-ons activated inside it and whether it was given to everyone.
//
// From spec.apps, without the components the platform places: those are not
// apps the tenant installed, and have no object in the store. An entry whose
// profile cannot be named is skipped rather than failing the reading,
// because one unresolvable install must not stop every other app's rights
// from being written.
func installedAppsOf(ctx context.Context, c client.Client, tenant *gentianov1alpha1.Tenant) ([]authz.InstalledApp, error) {
	installed, err := placement.Installed(ctx, c, tenant.Spec.Apps)
	if err != nil {
		return nil, err
	}
	var out []authz.InstalledApp
	for _, app := range installed {
		profile, err := catalogue.ResolveTenantComponentProfile(ctx, c, app)
		if err != nil {
			log.FromContext(ctx).Info("installed app not in the rights store: its profile cannot be named",
				"tenant", tenant.Name, "error", err.Error())
			continue
		}
		entry := authz.InstalledApp{Profile: profile, ForEveryone: app.DefaultGrant}
		for _, addon := range app.Addons {
			entry.Addons = append(entry.Addons, authz.InstalledApp{
				Profile: addon, ForEveryone: app.AddonDefaultGranted(addon),
			})
		}
		out = append(out, entry)
	}
	return out, nil
}

// positionsOf is the tenant's positions: the three every tenant has, for
// its installed apps, and the ones its spec declares. A declared position
// under one of the three names is left out; admission refuses it, and the
// three are not a tenant's to redefine.
func positionsOf(tenant *gentianov1alpha1.Tenant, apps []authz.InstalledApp) []authz.PositionSpec {
	out := authz.ShippedPositions(apps)
	for _, p := range tenant.Spec.Positions {
		if authz.Shipped(p.Name) {
			continue
		}
		spec := authz.PositionSpec{Name: p.Name, Includes: p.Includes}
		for _, role := range p.Roles {
			spec.Roles = append(spec.Roles, authz.RoleGrant{App: role.App, Role: role.Role})
		}
		out = append(out, spec)
	}
	return out
}
