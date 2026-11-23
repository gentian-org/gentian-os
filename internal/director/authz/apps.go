/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// App is the object an installed app is in the store: app:<tenant>/<profile>.
func App(tenant, profile string) string { return "app:" + tenant + "/" + profile }

// DeclaredRole is a role an app's profile declares beside user and admin.
type DeclaredRole struct {
	Name string
	// Includes are roles of the same app whose holders this role's holders
	// are too: manager includes salesman.
	Includes []string
}

// InstalledApp is one app of a tenant and the add-ons activated inside it,
// by profile name.
type InstalledApp struct {
	Profile string
	// ForEveryone says the tenant gave the app to every person of the
	// tenant: the position User holds its role user (ShippedPositions).
	ForEveryone bool
	// Positions are the further positions the tenant gave the app to: each
	// holds its role user. A name that is no position of the tenant gives
	// nothing.
	Positions []string
	// Roles are the further roles the app's profile declares.
	Roles []DeclaredRole
	// Addons are the add-ons activated inside the app. An add-on is an app
	// of its own, with roles of its own; an add-on's Addons are not read.
	Addons []InstalledApp
}

// appRelations are the relations on an app's object that say what the app
// is, and roleRelations the same for a declared role's: the projection's
// alone, so an entry under one that it would not write is removed. Who holds
// a role is none of them.
var (
	appRelations  = map[string]bool{"tenant": true, "base": true, "addon": true, "declared": true}
	roleRelations = map[string]bool{"app": true, "included_in": true}
)

// appTuples is what one tenant's installed apps are in the store: each app
// and each add-on attached to the tenant, an add-on tied to its base both
// ways, and every declared role tied to its app.
//
// Nothing here says who may use an app. That is a role somebody holds on it,
// through a position or directly.
func appTuples(tenant string, apps []InstalledApp) (map[string]Tuple, error) {
	want := map[string]Tuple{}
	add := func(t Tuple) { want[key(t)] = t }
	one := func(app InstalledApp) error {
		if !ValidName(app.Profile) {
			return fmt.Errorf("tenant %s: %q is not an app's name", tenant, app.Profile)
		}
		object := App(tenant, app.Profile)
		add(Tuple{User: Tenant(tenant), Relation: "tenant", Object: object})
		declared := map[string]bool{}
		for _, role := range app.Roles {
			if !ValidName(role.Name) || role.Name == RoleUser || role.Name == RoleAdmin {
				return fmt.Errorf("tenant %s app %s: %q is not a role an app can declare", tenant, app.Profile, role.Name)
			}
			declared[role.Name] = true
		}
		for _, role := range app.Roles {
			r := Role(tenant, app.Profile, role.Name)
			add(Tuple{User: object, Relation: "app", Object: r})
			add(Tuple{User: r, Relation: "declared", Object: object})
			for _, included := range role.Includes {
				if !declared[included] || included == role.Name {
					return fmt.Errorf("tenant %s app %s: role %s includes %q, which is not another role of the app", tenant, app.Profile, role.Name, included)
				}
				add(Tuple{User: r, Relation: "included_in", Object: Role(tenant, app.Profile, included)})
			}
		}
		return nil
	}
	for _, app := range apps {
		if app.Profile == "" {
			continue
		}
		if err := one(app); err != nil {
			return nil, err
		}
		for _, addon := range app.Addons {
			if addon.Profile == "" {
				continue
			}
			if err := one(addon); err != nil {
				return nil, err
			}
			add(Tuple{User: App(tenant, app.Profile), Relation: "base", Object: App(tenant, addon.Profile)})
			add(Tuple{User: App(tenant, addon.Profile), Relation: "addon", Object: App(tenant, app.Profile)})
		}
	}
	return want, nil
}

// ReconcileApps makes the store hold exactly the installed apps of each
// tenant given: every app and add-on attached to its tenant, with the roles
// its profile declares, and nothing left of what an app that is no longer
// installed was.
//
// A tenant not in the map is not touched; a tenant in it with no apps has
// every such entry removed. Who holds a role on an app is not this
// function's and stays, with one exception: a declared role that is no
// longer declared takes its holders with it, since nothing is left to hold.
func (c *OpenFGA) ReconcileApps(ctx context.Context, installed map[string][]InstalledApp) error {
	tenants := make([]string, 0, len(installed))
	for t := range installed {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)

	var writes, deletes []Tuple
	for _, tenant := range tenants {
		want, err := appTuples(tenant, installed[tenant])
		if err != nil {
			return err
		}
		wantedRoles := map[string]bool{}
		for _, t := range want {
			if t.Relation == "app" {
				wantedRoles[t.Object] = true
			}
		}
		// Every app the store attaches to this tenant, installed or not.
		attached, err := c.Read(ctx, Tuple{User: Tenant(tenant), Relation: "tenant", Object: "app:"})
		if err != nil {
			return fmt.Errorf("read apps of tenant %s: %w", tenant, err)
		}
		apps := map[string]bool{}
		for _, t := range attached {
			// An object under another tenant's name attached to this tenant
			// is not one this function wrote, and is not its to remove.
			if ofTenant(t.Object, "app:", tenant) {
				apps[t.Object] = true
			}
		}
		for _, t := range want {
			if strings.HasPrefix(t.Object, "app:") {
				apps[t.Object] = true
			}
		}
		installedApps := map[string]bool{}
		for _, t := range want {
			if strings.HasPrefix(t.Object, "app:") {
				installedApps[t.Object] = true
			}
		}
		roles := map[string]bool{}
		for object := range wantedRoles {
			roles[object] = true
		}
		for object := range apps {
			have, err := c.Read(ctx, Tuple{Object: object})
			if err != nil {
				return fmt.Errorf("read %s: %w", object, err)
			}
			for _, t := range have {
				p := plain(t)
				if p.Relation == "declared" && ofTenant(p.User, "role:", tenant) {
					roles[p.User] = true
				}
				// What was derived on an app that is no longer installed
				// goes with it: derivation reads installed apps only, and
				// nobody else removes it.
				if IsDerived(p.Relation) && !installedApps[object] {
					deletes = append(deletes, p)
					continue
				}
				if !appRelations[p.Relation] {
					continue
				}
				if _, keep := want[key(p)]; keep {
					delete(want, key(p))
					continue
				}
				deletes = append(deletes, p)
			}
		}
		for object := range roles {
			have, err := c.Read(ctx, Tuple{Object: object})
			if err != nil {
				return fmt.Errorf("read %s: %w", object, err)
			}
			for _, t := range have {
				p := plain(t)
				if !roleRelations[p.Relation] {
					if !wantedRoles[object] {
						deletes = append(deletes, p)
					}
					continue
				}
				if _, keep := want[key(p)]; keep {
					delete(want, key(p))
					continue
				}
				deletes = append(deletes, p)
			}
		}
		for _, t := range want {
			writes = append(writes, t)
		}
	}
	if len(writes) == 0 && len(deletes) == 0 {
		return nil
	}
	sort.Slice(writes, func(i, j int) bool { return key(writes[i]) < key(writes[j]) })
	sort.Slice(deletes, func(i, j int) bool { return key(deletes[i]) < key(deletes[j]) })
	if err := c.Write(ctx, writes, deletes); err != nil {
		return fmt.Errorf("reconcile apps: %w", err)
	}
	c.log.InfoContext(ctx, "apps reconciled", "tenants", len(tenants), "written", len(writes), "removed", len(deletes))
	return nil
}
