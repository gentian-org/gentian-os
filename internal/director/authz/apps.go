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

// InstalledApp is one app of a tenant and the addons activated inside it, by
// profile name.
type InstalledApp struct {
	Profile string
	Addons  []string
}

// appGroup is the Keycloak group whose members may use an app:
// gentian:tenant:<t>:app:<profile>. The operator creates one per installed
// app and per activated addon, and a person is put in it or not.
func appGroup(tenant, profile string) (string, error) {
	return Group("gentian:tenant:" + tenant + ":app:" + profile)
}

// appTuples is what one tenant's installed apps are in the store.
//
// The rule is not new. Entitlement is membership of the app's own group and
// nothing else; it was computed by the portal from the groups in a person's
// token, and is stored here so that the tiles and, later, the route ask the
// same question. Two things are kept exactly as they were:
//
//   - An addon is an app of its own: its own object, entitled by its own
//     group.
//   - A base with activated addons is entitled by the ADDONS' groups and not
//     by its own. A base is entered for what is inside it, so somebody
//     entitled to none of its addons has nothing to do there.
//
// That an administrator's account sees administration tiles only is the
// model's (can_use: entitled but not admin from tenant), not a tuple.
func appTuples(tenant string, apps []InstalledApp) (map[string]Tuple, error) {
	want := map[string]Tuple{}
	add := func(profile string, entitledBy []string) error {
		object := App(tenant, profile)
		t := Tuple{User: Tenant(tenant), Relation: "tenant", Object: object}
		want[key(t)] = t
		for _, by := range entitledBy {
			group, err := appGroup(tenant, by)
			if err != nil {
				return fmt.Errorf("tenant %s app %s: %w", tenant, by, err)
			}
			e := Tuple{User: group + "#member", Relation: "entitled", Object: object}
			want[key(e)] = e
		}
		return nil
	}
	for _, app := range apps {
		if app.Profile == "" {
			continue
		}
		var addons []string
		for _, a := range app.Addons {
			if a != "" {
				addons = append(addons, a)
			}
		}
		base := []string{app.Profile}
		if len(addons) > 0 {
			base = addons
		}
		if err := add(app.Profile, base); err != nil {
			return nil, err
		}
		for _, a := range addons {
			if err := add(a, []string{a}); err != nil {
				return nil, err
			}
		}
	}
	return want, nil
}

// ReconcileApps makes the store hold exactly the installed apps of each
// tenant given: every app attached to its tenant and entitled by its groups,
// and nothing left of an app that is no longer installed.
//
// A tenant not in the map is not touched; a tenant in it with no apps has
// every app tuple removed. Only the `tenant` and `entitled` relations are
// this function's: anything else stored on an app object is somebody else's
// act and stays.
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
		// Every app the store attaches to this tenant, installed or not.
		attached, err := c.Read(ctx, Tuple{User: Tenant(tenant), Relation: "tenant", Object: "app:"})
		if err != nil {
			return fmt.Errorf("read apps of tenant %s: %w", tenant, err)
		}
		objects := map[string]bool{}
		for _, t := range attached {
			// An object under another tenant's name attached to this tenant
			// is not one this function wrote, and is not its to remove.
			if strings.HasPrefix(t.Object, "app:"+tenant+"/") {
				objects[t.Object] = true
			}
		}
		for _, t := range want {
			objects[t.Object] = true
		}
		for object := range objects {
			have, err := c.Read(ctx, Tuple{Object: object})
			if err != nil {
				return fmt.Errorf("read %s: %w", object, err)
			}
			for _, t := range have {
				if t.Relation != "tenant" && t.Relation != "entitled" {
					continue
				}
				plain := Tuple{User: t.User, Relation: t.Relation, Object: t.Object}
				if _, keep := want[key(plain)]; keep {
					delete(want, key(plain))
					continue
				}
				deletes = append(deletes, plain)
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
