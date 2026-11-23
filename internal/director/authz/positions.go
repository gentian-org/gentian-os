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
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Roles and positions.
//
// A role is a privilege on one thing: user and admin on a tenant and on an
// app, and the further roles an app's profile declares. A position is a
// named set of roles a kind of person holds, and is what a person is given.
//
// What a position IS -- its name, the roles it holds, the positions it
// includes -- is declared: four every tenant has (ShippedPositions), and
// the ones a tenant's entry in git lists. The operator writes that into the
// store and removes what is no longer declared (ReconcilePositions). WHO
// holds a position, or a role beside one, is not declared anywhere: it is
// written when it is given, lives in the store alone, and travels in the
// tenant's backup (StoreOnlyOf).

// The two roles every tenant and app has.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// The positions every tenant has.
const (
	// PositionUser is a person of the tenant: user of the tenant, which lets
	// them in to its desktop, and of no app.
	PositionUser = "user"
	// PositionAllApps uses every app and add-on installed in the tenant, and
	// includes user.
	PositionAllApps = "all-apps"
	// PositionAppAdministrator administers every installed app.
	PositionAppAdministrator = "app-administrator"
	// PositionTenantAdministrator administers the tenant, and holds no role
	// on any app.
	PositionTenantAdministrator = "tenant-administrator"
)

// nameRE is the name of a position or of a declared role: a DNS label, so it
// needs no encoding and cannot carry the store's separators.
var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidName reports whether name can be a position's or a declared role's.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Position is the object a tenant's position is in the store:
// position:<tenant>/<name>.
func Position(tenant, name string) string { return "position:" + tenant + "/" + name }

// Role is the object a role an app declares is in the store:
// role:<tenant>/<profile>/<name>.
func Role(tenant, profile, name string) string {
	return "role:" + tenant + "/" + profile + "/" + name
}

// Holders is whoever holds a position or a declared role, as the holder of
// something else: position:<t>/<n>#holder.
func Holders(object string) string { return object + "#holder" }

// RoleGrant is one role a position holds: Role on the app App, or on the
// tenant itself when App is empty. On the tenant only user and admin exist.
type RoleGrant struct {
	App  string
	Role string
}

// PositionSpec is a position as it is declared.
type PositionSpec struct {
	// Name is the position's name within its tenant.
	Name string
	// Includes are positions of the same tenant whose roles this one holds
	// too: Executive includes Sales manager.
	Includes []string
	// Roles are the roles the position holds itself.
	Roles []RoleGrant
}

// ShippedPositions are the positions every tenant has, for its installed
// apps: User, All apps, App administrator and Tenant administrator. A
// tenant's own declaration cannot redefine them.
//
// User holds the role user on the tenant and nothing else. All apps
// includes User and holds user on every installed app and add-on, so it
// follows the installed set. The administrator of a base app administers
// its add-ons (the model), so App administrator holds admin on every app
// that is not an add-on.
func ShippedPositions(apps []InstalledApp) []PositionSpec {
	user := PositionSpec{Name: PositionUser, Roles: []RoleGrant{{Role: RoleUser}}}
	allApps := PositionSpec{Name: PositionAllApps, Includes: []string{PositionUser}}
	appAdmin := PositionSpec{Name: PositionAppAdministrator}
	tenantAdmin := PositionSpec{Name: PositionTenantAdministrator, Roles: []RoleGrant{{Role: RoleAdmin}}}
	for _, app := range apps {
		if app.Profile == "" {
			continue
		}
		allApps.Roles = append(allApps.Roles, RoleGrant{App: app.Profile, Role: RoleUser})
		appAdmin.Roles = append(appAdmin.Roles, RoleGrant{App: app.Profile, Role: RoleAdmin})
		// App administrator holds admin on the app, which is user too.
		if app.GivenTo(PositionTenantAdministrator) {
			tenantAdmin.Roles = append(tenantAdmin.Roles, RoleGrant{App: app.Profile, Role: RoleUser})
		}
		for _, addon := range app.Addons {
			if addon.Profile != "" {
				allApps.Roles = append(allApps.Roles, RoleGrant{App: addon.Profile, Role: RoleUser})
			}
		}
	}
	return []PositionSpec{user, allApps, appAdmin, tenantAdmin}
}

// GivenTo reports whether the tenant gave the app to a position by name.
func (a InstalledApp) GivenTo(position string) bool {
	for _, name := range a.Positions {
		if name == position {
			return true
		}
	}
	return false
}

// WithAppsGivenTo is a tenant's own position with the role user of every
// app the tenant gave to it, beside the roles it declares. A role it
// already declares is not added twice.
func WithAppsGivenTo(position PositionSpec, apps []InstalledApp) PositionSpec {
	has := map[RoleGrant]bool{}
	for _, role := range position.Roles {
		has[role] = true
	}
	for _, app := range apps {
		grant := RoleGrant{App: app.Profile, Role: RoleUser}
		if app.Profile == "" || has[grant] || !app.GivenTo(position.Name) {
			continue
		}
		has[grant] = true
		position.Roles = append(position.Roles, grant)
	}
	return position
}

// Shipped reports whether name is one of the positions every tenant has.
func Shipped(name string) bool {
	switch name {
	case PositionUser, PositionAllApps, PositionAppAdministrator, PositionTenantAdministrator:
		return true
	}
	return false
}

// grantTuple is a role a position holds, as the store's entry.
func grantTuple(tenant, position string, g RoleGrant) (Tuple, error) {
	holders := Holders(Position(tenant, position))
	if g.App == "" {
		if g.Role != RoleUser && g.Role != RoleAdmin {
			return Tuple{}, fmt.Errorf("position %s: a tenant has the roles user and admin, not %q", position, g.Role)
		}
		return Tuple{User: holders, Relation: g.Role, Object: Tenant(tenant)}, nil
	}
	if !ValidName(g.App) {
		return Tuple{}, fmt.Errorf("position %s: %q is not an app's name", position, g.App)
	}
	if g.Role == RoleUser || g.Role == RoleAdmin {
		return Tuple{User: holders, Relation: g.Role, Object: App(tenant, g.App)}, nil
	}
	if !ValidName(g.Role) {
		return Tuple{}, fmt.Errorf("position %s: %q is not a role's name", position, g.Role)
	}
	return Tuple{User: holders, Relation: "holder", Object: Role(tenant, g.App, g.Role)}, nil
}

// positionTuples is what a tenant's declared positions are in the store:
// each attached to its tenant, what it includes, and the roles it holds.
//
// A declaration the store cannot hold is refused whole, naming what is wrong
// with it: a name that is not one, a position declared twice, one that
// includes a position nobody declared, and a circle of positions that
// include each other.
func positionTuples(tenant string, positions []PositionSpec) (map[string]Tuple, error) {
	declared := map[string]PositionSpec{}
	for _, p := range positions {
		if !ValidName(p.Name) {
			return nil, fmt.Errorf("tenant %s: %q is not a position's name", tenant, p.Name)
		}
		if _, twice := declared[p.Name]; twice {
			return nil, fmt.Errorf("tenant %s: position %s is declared twice", tenant, p.Name)
		}
		declared[p.Name] = p
	}
	want := map[string]Tuple{}
	add := func(t Tuple) { want[key(t)] = t }
	for _, p := range positions {
		object := Position(tenant, p.Name)
		add(Tuple{User: Tenant(tenant), Relation: "tenant", Object: object})
		for _, included := range p.Includes {
			if _, ok := declared[included]; !ok {
				return nil, fmt.Errorf("tenant %s: position %s includes %q, which is not a position of the tenant", tenant, p.Name, included)
			}
			if included == p.Name {
				return nil, fmt.Errorf("tenant %s: position %s includes itself", tenant, p.Name)
			}
			add(Tuple{User: object, Relation: "included_in", Object: Position(tenant, included)})
		}
		for _, g := range p.Roles {
			t, err := grantTuple(tenant, p.Name, g)
			if err != nil {
				return nil, fmt.Errorf("tenant %s: %w", tenant, err)
			}
			add(t)
		}
	}
	if circle := inclusionCircle(declared); circle != "" {
		return nil, fmt.Errorf("tenant %s: positions include each other in a circle: %s", tenant, circle)
	}
	return want, nil
}

// inclusionCircle names a circle among the positions' inclusions, or "".
func inclusionCircle(declared map[string]PositionSpec) string {
	const (
		unseen = iota
		open
		done
	)
	state := map[string]int{}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	var path []string
	var visit func(name string) string
	visit = func(name string) string {
		switch state[name] {
		case done:
			return ""
		case open:
			for i, on := range path {
				if on == name {
					return strings.Join(append(append([]string{}, path[i:]...), name), " includes ")
				}
			}
			return name
		}
		state[name] = open
		path = append(path, name)
		for _, included := range declared[name].Includes {
			if circle := visit(included); circle != "" {
				return circle
			}
		}
		path = path[:len(path)-1]
		state[name] = done
		return ""
	}
	for _, name := range names {
		if circle := visit(name); circle != "" {
			return circle
		}
	}
	return ""
}

// rolesHeldOn are the kinds of object a position holds roles on.
var rolesHeldOn = []string{"tenant:", "app:", "role:"}

// ofTenant reports whether an object of kind (with its colon) is one of
// tenant's own: the tenant itself, or a thing named <tenant>/...
func ofTenant(object, kind, tenant string) bool {
	if kind == "tenant:" {
		return object == Tenant(tenant)
	}
	return strings.HasPrefix(object, kind+tenant+"/") && len(object) > len(kind+tenant+"/")
}

// ReconcilePositions makes the store hold exactly the positions declared for
// each tenant given: what each is, and nothing left of one that is no longer
// declared.
//
// Declared is each tenant's own positions; the four every tenant has are
// the caller's to add (ShippedPositions). A tenant not in the map is not
// touched. Who holds a position is not this function's and stays -- except
// for a position that is no longer declared, whose holders go with it:
// nothing would be left for them to hold.
//
// A tenant whose declaration the store cannot hold is left as it is and
// named in the error; the others are reconciled.
func (c *OpenFGA) ReconcilePositions(ctx context.Context, declared map[string][]PositionSpec) error {
	tenants := make([]string, 0, len(declared))
	for t := range declared {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)

	var writes, deletes []Tuple
	var refused []error
	for _, tenant := range tenants {
		want, err := positionTuples(tenant, declared[tenant])
		if err != nil {
			refused = append(refused, err)
			continue
		}
		wanted := map[string]bool{}
		for _, p := range declared[tenant] {
			wanted[Position(tenant, p.Name)] = true
		}
		attached, err := c.Read(ctx, Tuple{User: Tenant(tenant), Relation: "tenant", Object: "position:"})
		if err != nil {
			return fmt.Errorf("read positions of tenant %s: %w", tenant, err)
		}
		objects := map[string]bool{}
		for object := range wanted {
			objects[object] = true
		}
		for _, t := range attached {
			if ofTenant(t.Object, "position:", tenant) {
				objects[t.Object] = true
			}
		}
		names := make([]string, 0, len(objects))
		for object := range objects {
			names = append(names, object)
		}
		sort.Strings(names)
		for _, object := range names {
			// What the position is: its tenant, what includes it, and, when
			// it is gone, who held it.
			on, err := c.Read(ctx, Tuple{Object: object})
			if err != nil {
				return fmt.Errorf("read %s: %w", object, err)
			}
			for _, t := range on {
				p := plain(t)
				if p.Relation == "holder" {
					if !wanted[object] {
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
			// The roles it holds, on the tenant's own things.
			for _, kind := range rolesHeldOn {
				held, err := c.Read(ctx, Tuple{User: Holders(object), Object: kind})
				if err != nil {
					return fmt.Errorf("read what %s holds: %w", object, err)
				}
				for _, t := range held {
					p := plain(t)
					// What follows from what the position holds is the
					// derivation's to keep (derived.go), not this one's.
					if !ofTenant(p.Object, kind, tenant) || IsDerived(p.Relation) {
						continue
					}
					if _, keep := want[key(p)]; keep {
						delete(want, key(p))
						continue
					}
					deletes = append(deletes, p)
				}
			}
		}
		for _, t := range want {
			writes = append(writes, t)
		}
	}
	if len(writes) > 0 || len(deletes) > 0 {
		sort.Slice(writes, func(i, j int) bool { return key(writes[i]) < key(writes[j]) })
		sort.Slice(deletes, func(i, j int) bool { return key(deletes[i]) < key(deletes[j]) })
		if err := c.Write(ctx, writes, deletes); err != nil {
			return fmt.Errorf("reconcile positions: %w", err)
		}
		c.log.InfoContext(ctx, "positions reconciled", "tenants", len(tenants), "written", len(writes), "removed", len(deletes))
	}
	return errors.Join(refused...)
}

// ValidatePositions reports why a tenant's own positions cannot be declared
// as given, or nil: together with the four every tenant has, which a
// tenant's own may include and may not redefine.
func ValidatePositions(tenant string, own []PositionSpec) error {
	for _, p := range own {
		if Shipped(p.Name) {
			return fmt.Errorf("tenant %s: %s is a position every tenant has, and is not a tenant's to declare", tenant, p.Name)
		}
	}
	_, err := positionTuples(tenant, append(ShippedPositions(nil), own...))
	return err
}
