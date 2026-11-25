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

// What one person, or one position, holds in a tenant, and through what.
//
// A view (view.go) starts at an object and says who holds each role on it.
// This starts at the other end: a person, the positions they hold, and every
// role they hold on the tenant and on each of its apps, with the position it
// is held through. Then the store is asked what is in effect for them,
// because more is in effect than is held: the admin of an app holds every
// role the app declares, a role on an add-on lets a person into its base,
// and some permissions follow from the tenant or the cluster.
//
// Read-only, and from the store alone: what is shown is what a check would
// answer.

// Where a permission comes from.
const (
	// FromRole: a role the person holds on this object carries it.
	FromRole = "role"
	// FromTenant: on an app, it follows from what the person holds on the
	// tenant.
	FromTenant = "tenant"
	// FromApp: on an app, it follows from what the person holds on its base
	// or on one of its add-ons.
	FromApp = "app"
	// FromElsewhere: in effect, and nothing read here accounts for it.
	FromElsewhere = "elsewhere"
)

// Held is one role held on one object.
type Held struct {
	// Role is the role: user, admin, or the name of one the app declares.
	Role string `json:"role"`
	// Declared is true for a role the app's profile declares.
	Declared bool `json:"declared,omitempty"`
	// Position is the position the role is held through. Empty for a role
	// the person was given directly.
	Position string `json:"position,omitempty"`
	// Direct is true for a role the person holds as themselves, beside
	// their positions.
	Direct bool `json:"direct,omitempty"`
	// Carries are the permissions the role carries on this kind of object.
	Carries []string `json:"carries"`
}

// Permission is one permission on one object, for one person.
type Permission struct {
	Name string `json:"name"`
	// InEffect is the store's own answer for this person.
	InEffect bool `json:"inEffect"`
	// From says where it comes from, one of the From constants; empty for
	// one the person does not have.
	From string `json:"from,omitempty"`
}

// ObjectAccess is what a person holds on one object.
type ObjectAccess struct {
	// Object is the OpenFGA object: tenant:<t>, or app:<t>/<profile>.
	Object string `json:"object"`
	// App is the profile, for an app.
	App string `json:"app,omitempty"`
	// Held are the roles the person was given here, each with how.
	Held []Held `json:"held"`
	// Roles are the roles in effect here: the held ones, and those that
	// follow from them. It is what the app is told the person is.
	Roles       []string     `json:"roles"`
	Permissions []Permission `json:"permissions"`
}

// PositionHeld is one position a person holds.
type PositionHeld struct {
	Name string `json:"name"`
	// Through is the position the person was given that includes this one.
	// Empty for a position the person was given itself.
	Through string `json:"through,omitempty"`
}

// PersonAccess is what one person holds in one tenant.
type PersonAccess struct {
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`
	// Positions are the tenant's positions the person holds: the ones they
	// were given, and the ones those include.
	Positions []PositionHeld `json:"positions"`
	// OnTenant is the tenant itself; Apps are its apps, every one, also
	// those the person holds nothing on.
	OnTenant ObjectAccess   `json:"onTenant"`
	Apps     []ObjectAccess `json:"apps"`
}

// PositionHold is one role a position holds on one object.
type PositionHold struct {
	Object   string   `json:"object"`
	App      string   `json:"app,omitempty"`
	Role     string   `json:"role"`
	Declared bool     `json:"declared,omitempty"`
	Carries  []string `json:"carries"`
	// Through is, on a role inherited from an included position, the chain
	// of positions it comes through: the first is included by this one, the
	// last holds the role itself. Empty on a role the position holds itself.
	Through []string `json:"through,omitempty"`
}

// PositionAccess is what one position is in one tenant.
type PositionAccess struct {
	Tenant   string `json:"tenant"`
	Position string `json:"position"`
	// Holds are the roles the position holds itself.
	Holds []PositionHold `json:"holds"`
	// Inherited are the roles its holders hold through the positions it
	// includes, each with the chain it comes through, the shortest one; a
	// role the position holds itself is not repeated here.
	Inherited []PositionHold `json:"inherited"`
	// Includes are the positions whose roles its holders hold too;
	// IncludedIn the positions whose holders hold this one.
	Includes   []string `json:"includes"`
	IncludedIn []string `json:"includedIn"`
	// Holders are the accounts that were given the position itself, by
	// subject.
	Holders []string `json:"holders"`
}

// AccessReader answers what a person or a position holds in a tenant.
type AccessReader interface {
	// PersonAccess reads what the account sub holds in tenant. cluster is
	// the cluster's id, read only to find roles the person holds by name;
	// empty leaves those unread.
	PersonAccess(ctx context.Context, requestID, cluster, tenant, sub string) (PersonAccess, error)
	// PositionAccess reads what the position of that name is in tenant.
	PositionAccess(ctx context.Context, tenant, position string) (PositionAccess, error)
	// AppAccess reads who holds which role on one app of tenant, the roles
	// its profile declares among them.
	AppAccess(ctx context.Context, tenant, profile string) (View, error)
	// AppRoles answers which roles the account sub holds on one app of
	// tenant, the ones that follow from what it was given included.
	AppRoles(ctx context.Context, requestID, tenant, profile, sub string) ([]string, error)
	// PositionsHeld answers the names of the tenant's positions the account
	// sub holds.
	PositionsHeld(ctx context.Context, tenant, sub string) ([]string, error)
}

// PersonAccess implements AccessReader.
func (c *OpenFGA) PersonAccess(ctx context.Context, requestID, cluster, tenant, sub string) (PersonAccess, error) {
	ask := func(ctx context.Context, questions []Question) ([]bool, error) {
		return c.BatchCheck(ctx, requestID, questions)
	}
	return personAccess(ctx, c.Read, ask, cluster, tenant, sub)
}

// PositionAccess implements AccessReader.
func (c *OpenFGA) PositionAccess(ctx context.Context, tenant, position string) (PositionAccess, error) {
	return positionAccess(ctx, c.Read, tenant, position)
}

// AppAccess implements AccessReader.
func (c *OpenFGA) AppAccess(ctx context.Context, tenant, profile string) (View, error) {
	if !ValidName(tenant) || !ValidName(profile) {
		return View{}, fmt.Errorf("%w: %q", ErrInvalidID, tenant+"/"+profile)
	}
	return appView(ctx, c.Read, tenant, profile)
}

// AppRoles implements AccessReader.
func (c *OpenFGA) AppRoles(ctx context.Context, requestID, tenant, profile, sub string) ([]string, error) {
	ask := func(ctx context.Context, questions []Question) ([]bool, error) {
		return c.BatchCheck(ctx, requestID, questions)
	}
	return appRoles(ctx, c.Read, ask, tenant, profile, sub)
}

// AppRoleSet is the roles a person holds on one app, or on one add-on.
type AppRoleSet struct {
	// App is the profile name.
	App string `json:"app"`
	// Roles are the roles held, sorted: user, admin and declared ones.
	Roles []string `json:"roles"`
}

// SignInRoles answers what a sign-in to an app carries: the roles the
// account sub holds on the app, and on each add-on activated in it, since
// an add-on's roles are used inside its base. The app first, its add-ons
// after it by name; an app or add-on the person holds nothing on is left
// out, and an app the tenant has not answers nothing.
func (c *OpenFGA) SignInRoles(ctx context.Context, requestID, tenant, profile, sub string) ([]AppRoleSet, error) {
	ask := func(ctx context.Context, questions []Question) ([]bool, error) {
		return c.BatchCheck(ctx, requestID, questions)
	}
	return signInRoles(ctx, c.Read, ask, tenant, profile, sub)
}

func signInRoles(ctx context.Context, read readFunc, ask askFunc, tenant, profile, sub string) ([]AppRoleSet, error) {
	out := []AppRoleSet{}
	own, err := appRoles(ctx, read, ask, tenant, profile, sub)
	if err != nil {
		return nil, err
	}
	if len(own) > 0 {
		out = append(out, AppRoleSet{App: profile, Roles: own})
	}
	base := App(tenant, profile)
	tuples, err := read(ctx, Tuple{User: base, Relation: "base", Object: "app:"})
	if err != nil {
		return nil, fmt.Errorf("read the add-ons of %s: %w", base, err)
	}
	addons := make([]string, 0, len(tuples))
	for _, t := range tuples {
		if ofTenant(t.Object, "app:", tenant) {
			addons = append(addons, profileOf(t.Object, tenant))
		}
	}
	sort.Strings(addons)
	for _, addon := range addons {
		roles, err := appRoles(ctx, read, ask, tenant, addon, sub)
		if err != nil {
			return nil, err
		}
		if len(roles) > 0 {
			out = append(out, AppRoleSet{App: addon, Roles: roles})
		}
	}
	return out, nil
}

type (
	readFunc func(context.Context, Tuple) ([]Tuple, error)
	askFunc  func(context.Context, []Question) ([]bool, error)
)

// appsOf lists a tenant's app objects, sorted.
func appsOf(ctx context.Context, read readFunc, tenant string) ([]string, error) {
	tuples, err := read(ctx, Tuple{User: Tenant(tenant), Relation: "tenant", Object: "app:"})
	if err != nil {
		return nil, fmt.Errorf("read the apps of %s: %w", tenant, err)
	}
	apps := make([]string, 0, len(tuples))
	for _, t := range tuples {
		// The store is the platform's alone, but an app object named for
		// another tenant would be shown as this one's. Keep to the name.
		if ofTenant(t.Object, "app:", tenant) {
			apps = append(apps, t.Object)
		}
	}
	sort.Strings(apps)
	return apps, nil
}

// declaredRolesOf lists the role objects an app's profile declares, sorted.
func declaredRolesOf(ctx context.Context, read readFunc, tenant, app string) ([]string, error) {
	tuples, err := read(ctx, Tuple{User: app, Relation: "app", Object: "role:"})
	if err != nil {
		return nil, fmt.Errorf("read the roles of %s: %w", app, err)
	}
	roles := make([]string, 0, len(tuples))
	for _, t := range tuples {
		if strings.HasPrefix(t.Object, "role:"+strings.TrimPrefix(app, "app:")+"/") {
			roles = append(roles, t.Object)
		}
	}
	sort.Strings(roles)
	return roles, nil
}

// profileOf is the profile an app object is named for.
func profileOf(object, tenant string) string {
	return strings.TrimPrefix(object, "app:"+tenant+"/")
}

// roleNameOf is the name of a declared role's object.
func roleNameOf(object string) string {
	return object[strings.LastIndex(object, "/")+1:]
}

// positionNameOf is the name of a tenant's position object.
func positionNameOf(object, tenant string) string {
	return strings.TrimPrefix(object, "position:"+tenant+"/")
}

// positionsHeld reads the tenant's positions a person holds: the ones given
// to them, and every one those include, each with the given position it
// comes through. Keyed by the position's object.
func positionsHeld(ctx context.Context, read readFunc, tenant, user string) (map[string]PositionHeld, error) {
	given, err := read(ctx, Tuple{User: user, Relation: "holder", Object: "position:"})
	if err != nil {
		return nil, fmt.Errorf("read the positions of %s: %w", user, err)
	}
	held := map[string]PositionHeld{}
	var queue []string
	for _, t := range given {
		if !ofTenant(t.Object, "position:", tenant) {
			continue
		}
		held[t.Object] = PositionHeld{Name: positionNameOf(t.Object, tenant)}
		queue = append(queue, t.Object)
	}
	sort.Strings(queue)
	// A position's holders hold every position it is included in the
	// holders of: executive included_in sales-manager.
	for len(queue) > 0 {
		object := queue[0]
		queue = queue[1:]
		includes, err := read(ctx, Tuple{User: object, Relation: "included_in", Object: "position:"})
		if err != nil {
			return nil, fmt.Errorf("read what %s includes: %w", object, err)
		}
		through := held[object].Through
		if through == "" {
			through = held[object].Name
		}
		for _, t := range includes {
			if _, seen := held[t.Object]; seen || !ofTenant(t.Object, "position:", tenant) {
				continue
			}
			held[t.Object] = PositionHeld{Name: positionNameOf(t.Object, tenant), Through: through}
			queue = append(queue, t.Object)
		}
	}
	return held, nil
}

// PositionsHeld answers the names of the tenant's positions the account sub
// holds, the ones that follow from an inclusion among them. Sorted.
func (c *OpenFGA) PositionsHeld(ctx context.Context, tenant, sub string) ([]string, error) {
	user, err := User(sub)
	if err != nil {
		return nil, err
	}
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidID, tenant)
	}
	held, err := positionsHeld(ctx, c.Read, tenant, user)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(held))
	for _, p := range held {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names, nil
}

// HoldsAnyPosition reports whether the account sub holds one of the named
// positions of tenant: given to them, or through a position given to them
// that includes it.
func HoldsAnyPosition(ctx context.Context, s Store, tenant, sub string, positions []string) (bool, error) {
	user, err := User(sub)
	if err != nil {
		return false, err
	}
	if !ValidName(tenant) {
		return false, fmt.Errorf("%w: %q", ErrInvalidID, tenant)
	}
	if len(positions) == 0 {
		return false, nil
	}
	held, err := positionsHeld(ctx, s.Read, tenant, user)
	if err != nil {
		return false, err
	}
	for _, p := range positions {
		if _, ok := held[Position(tenant, p)]; ok {
			return true, nil
		}
	}
	return false, nil
}

func personAccess(ctx context.Context, read readFunc, ask askFunc, cluster, tenant, sub string) (PersonAccess, error) {
	user, err := User(sub)
	if err != nil {
		return PersonAccess{}, err
	}
	g, err := parseModel(modelV1)
	if err != nil {
		return PersonAccess{}, err
	}
	out := PersonAccess{Tenant: tenant, Subject: sub, Positions: []PositionHeld{}, Apps: []ObjectAccess{}}

	positions, err := positionsHeld(ctx, read, tenant, user)
	if err != nil {
		return PersonAccess{}, err
	}
	through := map[string]string{} // position:<t>/<n>#holder -> the position's name
	for object, p := range positions {
		through[Holders(object)] = p.Name
		out.Positions = append(out.Positions, p)
	}
	sort.Slice(out.Positions, func(a, b int) bool { return out.Positions[a].Name < out.Positions[b].Name })

	apps, err := appsOf(ctx, read, tenant)
	if err != nil {
		return PersonAccess{}, err
	}
	objects := append([]string{Tenant(tenant)}, apps...)

	// A role held by name is stored for user:<realm>/<sub>, and only a check
	// asked as that user sees it. The realm is not known here, and need not
	// be: the tuples say which such users end in this subject.
	byName := func(u string) bool {
		rest, ok := strings.CutPrefix(u, "user:")
		return ok && strings.HasSuffix(rest, "/"+strings.TrimPrefix(user, "user:"))
	}
	identities := map[string]bool{user: true}
	held := map[string][]Held{}
	// how says how the person holds an entry, or that they do not.
	how := func(holder string) (h Held, ok bool) {
		switch name, is := through[holder]; {
		case is:
			h.Position = name
		case holder == user:
			h.Direct = true
		case byName(holder):
			h.Direct = true
			identities[holder] = true
		default:
			return h, false
		}
		return h, true
	}
	// beside[app] are the apps an app's permissions can follow from: its
	// base, and its add-ons.
	beside := map[string][]besideApp{}
	declared := map[string][]string{}
	scan := func(object string, keep bool) error {
		typ, _, _ := strings.Cut(object, ":")
		tuples, err := read(ctx, Tuple{Object: object})
		if err != nil {
			return fmt.Errorf("read %s: %w", object, err)
		}
		for _, t := range tuples {
			if typ == "app" && (t.Relation == "base" || t.Relation == "addon") && ofTenant(t.User, "app:", tenant) {
				beside[object] = append(beside[object], besideApp{app: t.User, edge: t.Relation})
			}
			if typ == "app" && t.Relation == "declared" && ofTenant(t.User, "role:", tenant) {
				declared[object] = append(declared[object], t.User)
			}
			if !g.role[typ][t.Relation] {
				continue
			}
			h, ok := how(t.User)
			if !ok {
				continue
			}
			h.Role = t.Relation
			h.Carries = g.grantsOf(typ, t.Relation)
			if h.Carries == nil {
				h.Carries = []string{}
			}
			if keep {
				held[object] = append(held[object], h)
			}
		}
		return nil
	}
	for _, object := range objects {
		if err := scan(object, true); err != nil {
			return PersonAccess{}, err
		}
	}
	// The roles an app declares, held on objects of their own. Holding one
	// is holding user on the app, so it carries what user carries.
	usersCarry := g.grantsOf("app", RoleUser)
	if usersCarry == nil {
		usersCarry = []string{}
	}
	for _, app := range apps {
		sort.Strings(declared[app])
		for _, role := range declared[app] {
			tuples, err := read(ctx, Tuple{Relation: "holder", Object: role})
			if err != nil {
				return PersonAccess{}, fmt.Errorf("read %s: %w", role, err)
			}
			for _, t := range tuples {
				h, ok := how(t.User)
				if !ok {
					continue
				}
				h.Role, h.Declared, h.Carries = roleNameOf(role), true, usersCarry
				held[app] = append(held[app], h)
			}
		}
	}
	if cluster != "" {
		// Read for the names alone: what the person holds on the cluster is
		// the cluster's to show.
		if err := scan(Cluster(cluster), false); err != nil {
			return PersonAccess{}, err
		}
	}
	users := make([]string, 0, len(identities))
	for u := range identities {
		users = append(users, u)
	}
	sort.Strings(users)

	// What is in effect: every permission and role of each object, and who
	// holds each role an app declares.
	type asked struct{ object, on, relation string }
	var asks []asked
	for _, object := range objects {
		typ, _, _ := strings.Cut(object, ":")
		for _, relation := range append(g.permissions(typ), g.roles(typ)...) {
			asks = append(asks, asked{object: object, on: object, relation: relation})
		}
		for _, role := range declared[object] {
			asks = append(asks, asked{object: object, on: role, relation: "holder"})
		}
	}
	var questions []Question
	for _, a := range asks {
		for _, u := range users {
			questions = append(questions, Question{User: u, Relation: a.relation, Object: a.on})
		}
	}
	answers, err := ask(ctx, questions)
	if err != nil {
		return PersonAccess{}, err
	}
	if len(answers) != len(questions) {
		return PersonAccess{}, fmt.Errorf("asked %d questions, answered %d", len(questions), len(answers))
	}
	inEffect := map[string]map[string]bool{}
	rolesInEffect := map[string]map[string]bool{}
	for i, a := range asks {
		yes := false
		for _, answer := range answers[i*len(users) : (i+1)*len(users)] {
			yes = yes || answer
		}
		if !yes {
			continue
		}
		name := a.relation
		if a.on != a.object {
			name = roleNameOf(a.on)
		}
		typ, _, _ := strings.Cut(a.object, ":")
		if a.on != a.object || g.role[typ][a.relation] {
			if rolesInEffect[a.object] == nil {
				rolesInEffect[a.object] = map[string]bool{}
			}
			rolesInEffect[a.object][name] = true
		}
		if a.on == a.object {
			if inEffect[a.object] == nil {
				inEffect[a.object] = map[string]bool{}
			}
			inEffect[a.object][a.relation] = true
		}
	}

	onTenant := inEffect[Tenant(tenant)]
	for _, object := range objects {
		typ, _, _ := strings.Cut(object, ":")
		access := ObjectAccess{Object: object, Held: held[object], Roles: []string{}, Permissions: []Permission{}}
		if access.Held == nil {
			access.Held = []Held{}
		}
		sort.SliceStable(access.Held, func(a, b int) bool {
			if access.Held[a].Role != access.Held[b].Role {
				return access.Held[a].Role < access.Held[b].Role
			}
			return access.Held[a].Position < access.Held[b].Position
		})
		for role := range rolesInEffect[object] {
			access.Roles = append(access.Roles, role)
		}
		sort.Strings(access.Roles)
		carried := map[string]bool{}
		for _, h := range access.Held {
			for _, p := range h.Carries {
				carried[p] = true
			}
		}
		for _, name := range g.permissions(typ) {
			p := Permission{Name: name, InEffect: inEffect[object][name]}
			switch {
			case !p.InEffect:
			case carried[name]:
				p.From = FromRole
			case typ == "app" && g.followsFrom(typ, name, "tenant", onTenant):
				p.From = FromTenant
			case typ == "app" && followsFromBeside(g, name, beside[object], inEffect):
				p.From = FromApp
			default:
				p.From = FromElsewhere
			}
			access.Permissions = append(access.Permissions, p)
		}
		if typ == "app" {
			access.App = profileOf(object, tenant)
			out.Apps = append(out.Apps, access)
		} else {
			out.OnTenant = access
		}
	}
	return out, nil
}

// besideApp is an app another one's permissions can follow from: its base
// (edge "base") or one of its add-ons (edge "addon").
type besideApp struct {
	app, edge string
}

// followsFromBeside reports whether a permission on an app follows from
// what is in effect on its base or on one of its add-ons: the base's admin
// administers it, and whoever holds a role on an add-on uses its base. The
// store keeps both as derived entries; this says where they come from.
func followsFromBeside(g *modelGraph, permission string, beside []besideApp, inEffect map[string]map[string]bool) bool {
	for _, other := range beside {
		there := inEffect[other.app]
		switch other.edge {
		case "base":
			if there[RoleAdmin] && g.reach("app", RoleAdmin)[permission] {
				return true
			}
		case "addon":
			if (there[RoleUser] || there[RoleAdmin]) && g.reach("app", RoleUser)[permission] {
				return true
			}
		}
	}
	return false
}

// followsFrom reports whether a permission of a type follows from something
// held on the object its edge names: "can_write_credential: admin from
// tenant" follows once admin is in effect on the tenant.
func (g *modelGraph) followsFrom(typ, permission, edge string, there map[string]bool) bool {
	for relation, refs := range g.parent[typ] {
		if !g.reach(typ, relation)[permission] {
			continue
		}
		for _, ref := range refs {
			if ref.Edge == edge && there[ref.Relation] {
				return true
			}
		}
	}
	return false
}

// appRoles answers which roles a person holds on one app: user, admin and
// the ones its profile declares, as the store answers for each. Sorted;
// empty for a person who holds none, and for an app the tenant has not.
func appRoles(ctx context.Context, read readFunc, ask askFunc, tenant, profile, sub string) ([]string, error) {
	user, err := User(sub)
	if err != nil {
		return nil, err
	}
	if !ValidName(tenant) || !ValidName(profile) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidID, tenant+"/"+profile)
	}
	app := App(tenant, profile)
	declared, err := declaredRolesOf(ctx, read, tenant, app)
	if err != nil {
		return nil, err
	}
	names := []string{RoleAdmin, RoleUser}
	questions := []Question{
		{User: user, Relation: RoleAdmin, Object: app},
		{User: user, Relation: RoleUser, Object: app},
	}
	for _, role := range declared {
		names = append(names, roleNameOf(role))
		questions = append(questions, Question{User: user, Relation: "holder", Object: role})
	}
	answers, err := ask(ctx, questions)
	if err != nil {
		return nil, err
	}
	if len(answers) != len(questions) {
		return nil, fmt.Errorf("asked %d questions, answered %d", len(questions), len(answers))
	}
	roles := []string{}
	for i, yes := range answers {
		if yes {
			roles = append(roles, names[i])
		}
	}
	sort.Strings(roles)
	return roles, nil
}

func positionAccess(ctx context.Context, read readFunc, tenant, position string) (PositionAccess, error) {
	if !ValidName(tenant) || !ValidName(position) {
		return PositionAccess{}, fmt.Errorf("%w: %q", ErrInvalidID, position)
	}
	object := Position(tenant, position)
	g, err := parseModel(modelV1)
	if err != nil {
		return PositionAccess{}, err
	}
	out := PositionAccess{
		Tenant: tenant, Position: position,
		Holds: []PositionHold{}, Inherited: []PositionHold{}, Includes: []string{}, IncludedIn: []string{}, Holders: []string{},
	}
	holds, err := positionHolds(ctx, read, g, tenant, object)
	if err != nil {
		return PositionAccess{}, err
	}
	out.Holds = holds
	inherited, err := inheritedHolds(ctx, read, g, tenant, object, holds)
	if err != nil {
		return PositionAccess{}, err
	}
	out.Inherited = inherited

	includes, err := read(ctx, Tuple{User: object, Relation: "included_in", Object: "position:"})
	if err != nil {
		return PositionAccess{}, fmt.Errorf("read what %s includes: %w", object, err)
	}
	for _, t := range includes {
		if ofTenant(t.Object, "position:", tenant) {
			out.Includes = append(out.Includes, positionNameOf(t.Object, tenant))
		}
	}
	sort.Strings(out.Includes)
	on, err := read(ctx, Tuple{Object: object})
	if err != nil {
		return PositionAccess{}, fmt.Errorf("read %s: %w", object, err)
	}
	for _, t := range on {
		switch {
		case t.Relation == "included_in" && ofTenant(t.User, "position:", tenant):
			out.IncludedIn = append(out.IncludedIn, positionNameOf(t.User, tenant))
		case t.Relation == "holder" && strings.HasPrefix(t.User, "user:") && !strings.Contains(t.User, "#"):
			out.Holders = append(out.Holders, strings.TrimPrefix(t.User, "user:"))
		}
	}
	sort.Strings(out.IncludedIn)
	sort.Strings(out.Holders)
	return out, nil
}

// positionHolds reads the roles the position object holds itself, sorted by
// object and role.
func positionHolds(ctx context.Context, read readFunc, g *modelGraph, tenant, object string) ([]PositionHold, error) {
	holds := []PositionHold{}
	usersCarry := g.grantsOf("app", RoleUser)
	for _, kind := range rolesHeldOn {
		typ := strings.TrimSuffix(kind, ":")
		tuples, err := read(ctx, Tuple{User: Holders(object), Object: kind})
		if err != nil {
			return nil, fmt.Errorf("read what %s holds: %w", object, err)
		}
		for _, t := range tuples {
			// Another tenant's object is that tenant's to show.
			if !ofTenant(t.Object, kind, tenant) {
				continue
			}
			hold := PositionHold{Object: t.Object, Role: t.Relation}
			switch typ {
			case "tenant":
				hold.Carries = g.grantsOf(typ, t.Relation)
			case "app":
				hold.App = profileOf(t.Object, tenant)
				hold.Carries = g.grantsOf(typ, t.Relation)
			case "role":
				if t.Relation != "holder" {
					continue
				}
				rest := strings.TrimPrefix(t.Object, "role:"+tenant+"/")
				app, name, _ := strings.Cut(rest, "/")
				hold.Object, hold.App, hold.Role, hold.Declared = App(tenant, app), app, name, true
				hold.Carries = usersCarry
			}
			if typ != "role" && !g.role[typ][t.Relation] {
				continue
			}
			if hold.Carries == nil {
				hold.Carries = []string{}
			}
			holds = append(holds, hold)
		}
	}
	sort.SliceStable(holds, func(a, b int) bool {
		if holds[a].Object != holds[b].Object {
			return holds[a].Object < holds[b].Object
		}
		return holds[a].Role < holds[b].Role
	})
	return holds, nil
}

// inheritedHolds reads the roles the position object's holders hold through
// the positions it includes, walking the inclusions breadth first so that
// each role names the shortest chain it comes through. own are the roles the
// position holds itself, which are not repeated.
func inheritedHolds(ctx context.Context, read readFunc, g *modelGraph, tenant, object string, own []PositionHold) ([]PositionHold, error) {
	key := func(h PositionHold) string { return h.Object + "|" + h.App + "|" + h.Role }
	seenRole := map[string]bool{}
	for _, h := range own {
		seenRole[key(h)] = true
	}
	out := []PositionHold{}
	type step struct {
		object string
		chain  []string
	}
	visited := map[string]bool{object: true}
	queue := []step{{object: object}}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		includes, err := read(ctx, Tuple{User: at.object, Relation: "included_in", Object: "position:"})
		if err != nil {
			return nil, fmt.Errorf("read what %s includes: %w", at.object, err)
		}
		var next []string
		for _, t := range includes {
			if ofTenant(t.Object, "position:", tenant) && !visited[t.Object] {
				next = append(next, t.Object)
			}
		}
		sort.Strings(next)
		for _, included := range next {
			visited[included] = true
			chain := append(append([]string{}, at.chain...), positionNameOf(included, tenant))
			holds, err := positionHolds(ctx, read, g, tenant, included)
			if err != nil {
				return nil, err
			}
			for _, h := range holds {
				if seenRole[key(h)] {
					continue
				}
				seenRole[key(h)] = true
				h.Through = chain
				out = append(out, h)
			}
			queue = append(queue, step{object: included, chain: chain})
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Object != out[b].Object {
			return out[a].Object < out[b].Object
		}
		return out[a].Role < out[b].Role
	})
	return out, nil
}
