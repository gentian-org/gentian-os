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
	"sort"
	"strings"
)

// Giving and taking.
//
// What a position is, is declared and projected (positions.go). Who holds
// one, and which role a person holds beside their positions, is written
// here, when it is given, and nowhere else: one entry in the store per
// assignment, with nothing in between.
//
// Every act names a tenant, and touches only that tenant's own things. What
// is given must exist: a position the tenant has, an app it has installed, a
// role that app declares. And one thing cannot be taken: the last
// administrator a tenant has of its own, because nobody would be left who
// may give the role back.

// What an assignment can be refused for.
var (
	// ErrNoSuchThing: the position, app or role named is not one the tenant
	// has.
	ErrNoSuchThing = errors.New("the tenant has no such position, app or role")
	// ErrLastAdministrator: the act would leave the tenant with no
	// administrator of its own.
	ErrLastAdministrator = errors.New("the tenant would be left with no administrator of its own")
)

// Assignment is one thing a person holds: a position, or a role beside
// their positions. Exactly one of Position and Role is set.
type Assignment struct {
	// Position is a position of the tenant.
	Position string `json:"position,omitempty"`
	// Role is user, admin, or a role App's profile declares.
	Role string `json:"role,omitempty"`
	// App is the app the role is held on; empty for a role on the tenant
	// itself.
	App string `json:"app,omitempty"`
}

// Store is the store as giving and taking use it.
type Store interface {
	Read(ctx context.Context, filter Tuple) ([]Tuple, error)
	Write(ctx context.Context, writes, deletes []Tuple) error
}

// assignmentTuple is an assignment as the store's entry, for a person of
// the tenant. It checks the names, not that the thing exists.
func assignmentTuple(tenant, user string, a Assignment) (Tuple, error) {
	if !ValidName(tenant) {
		return Tuple{}, fmt.Errorf("%w: tenant %q", ErrInvalidID, tenant)
	}
	switch {
	case a.Position != "" && a.Role == "" && a.App == "":
		if !ValidName(a.Position) {
			return Tuple{}, fmt.Errorf("%w: position %q", ErrInvalidID, a.Position)
		}
		return Tuple{User: user, Relation: "holder", Object: Position(tenant, a.Position)}, nil
	case a.Position == "" && a.Role != "" && a.App == "":
		if a.Role != RoleUser && a.Role != RoleAdmin {
			return Tuple{}, fmt.Errorf("%w: a tenant has the roles user and admin, not %q", ErrInvalidID, a.Role)
		}
		return Tuple{User: user, Relation: a.Role, Object: Tenant(tenant)}, nil
	case a.Position == "" && a.Role != "":
		if !ValidName(a.App) || !ValidName(a.Role) {
			return Tuple{}, fmt.Errorf("%w: role %q on app %q", ErrInvalidID, a.Role, a.App)
		}
		if a.Role == RoleUser || a.Role == RoleAdmin {
			return Tuple{User: user, Relation: a.Role, Object: App(tenant, a.App)}, nil
		}
		return Tuple{User: user, Relation: "holder", Object: Role(tenant, a.App, a.Role)}, nil
	}
	return Tuple{}, fmt.Errorf("%w: an assignment is a position, or a role", ErrInvalidID)
}

// exists reports whether what an assignment's entry is held on is one of
// the tenant's own things: a position it has, an app it has installed, a
// role that app declares.
func exists(ctx context.Context, s Store, tenant string, t Tuple) (bool, error) {
	var structure Tuple
	switch {
	case t.Object == Tenant(tenant):
		structure = Tuple{Relation: "cluster", Object: t.Object}
	case strings.HasPrefix(t.Object, "position:"), strings.HasPrefix(t.Object, "app:"):
		structure = Tuple{User: Tenant(tenant), Relation: "tenant", Object: t.Object}
	case strings.HasPrefix(t.Object, "role:"):
		rest := strings.TrimPrefix(t.Object, "role:"+tenant+"/")
		app, _, _ := strings.Cut(rest, "/")
		structure = Tuple{User: App(tenant, app), Relation: "app", Object: t.Object}
	default:
		return false, nil
	}
	have, err := s.Read(ctx, structure)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", t.Object, err)
	}
	return len(have) > 0, nil
}

// Give makes the account sub hold an assignment in tenant. Giving what is
// already held changes nothing and says so.
func Give(ctx context.Context, s Store, tenant, sub string, a Assignment) (changed bool, err error) {
	user, err := User(sub)
	if err != nil {
		return false, err
	}
	t, err := assignmentTuple(tenant, user, a)
	if err != nil {
		return false, err
	}
	ok, err := exists(ctx, s, tenant, t)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNoSuchThing, t.Object)
	}
	have, err := s.Read(ctx, t)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", t.Object, err)
	}
	if len(have) > 0 {
		return false, nil
	}
	if err := s.Write(ctx, []Tuple{t}, nil); err != nil {
		return false, fmt.Errorf("give %s on %s: %w", t.Relation, t.Object, err)
	}
	return true, nil
}

// Take makes the account sub no longer hold an assignment in tenant. Taking
// what is not held changes nothing and says so. The tenant's last
// administrator of its own keeps the role (ErrLastAdministrator).
func Take(ctx context.Context, s Store, tenant, sub string, a Assignment) (changed bool, err error) {
	user, err := User(sub)
	if err != nil {
		return false, err
	}
	t, err := assignmentTuple(tenant, user, a)
	if err != nil {
		return false, err
	}
	have, err := s.Read(ctx, t)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", t.Object, err)
	}
	if len(have) == 0 {
		return false, nil
	}
	if err := keepsAnAdministrator(ctx, s, tenant, []Tuple{t}); err != nil {
		return false, err
	}
	if err := s.Write(ctx, nil, []Tuple{t}); err != nil {
		return false, fmt.Errorf("take %s on %s: %w", t.Relation, t.Object, err)
	}
	return true, nil
}

// Assignments reads what the account sub was given in tenant: its positions, and
// the roles it holds beside them, positions first. Not what follows from
// those; that is PersonAccess's to say.
func Assignments(ctx context.Context, s Store, tenant, sub string) ([]Assignment, error) {
	user, err := User(sub)
	if err != nil {
		return nil, err
	}
	tuples, err := heldBy(ctx, s, tenant, user)
	if err != nil {
		return nil, err
	}
	out := make([]Assignment, 0, len(tuples))
	for _, t := range tuples {
		switch {
		case strings.HasPrefix(t.Object, "position:"):
			out = append(out, Assignment{Position: positionNameOf(t.Object, tenant)})
		case strings.HasPrefix(t.Object, "tenant:"):
			out = append(out, Assignment{Role: t.Relation})
		case strings.HasPrefix(t.Object, "app:"):
			out = append(out, Assignment{App: profileOf(t.Object, tenant), Role: t.Relation})
		case strings.HasPrefix(t.Object, "role:"):
			rest := strings.TrimPrefix(t.Object, "role:"+tenant+"/")
			app, name, _ := strings.Cut(rest, "/")
			out = append(out, Assignment{App: app, Role: name})
		}
	}
	// Positions first, then the roles beside them: on the tenant, then by
	// app.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Position != "") != (b.Position != "") {
			return a.Position != ""
		}
		if a.Position != b.Position {
			return a.Position < b.Position
		}
		if a.App != b.App {
			return a.App < b.App
		}
		return a.Role < b.Role
	})
	return out, nil
}

// heldBy reads the entries a user holds on the tenant's own things: the
// positions given to them and the roles beside. Sorted.
func heldBy(ctx context.Context, s Store, tenant, user string) ([]Tuple, error) {
	var out []Tuple
	for _, kind := range []string{"position:", "tenant:", "app:", "role:"} {
		tuples, err := s.Read(ctx, Tuple{User: user, Object: kind})
		if err != nil {
			return nil, fmt.Errorf("read what %s holds: %w", user, err)
		}
		for _, t := range tuples {
			p := plain(t)
			if !ofTenant(p.Object, kind, tenant) {
				continue
			}
			// On the tenant, only the two roles are given to a person here;
			// perimeter approver and auditor are the Cluster claim's.
			if kind == "tenant:" && p.Relation != RoleUser && p.Relation != RoleAdmin {
				continue
			}
			if (kind == "position:" || kind == "role:") && p.Relation != "holder" {
				continue
			}
			if kind == "app:" && p.Relation != RoleUser && p.Relation != RoleAdmin {
				continue
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out, nil
}

// RemovePerson takes everything the account sub holds in tenant: the person
// left, or was removed. It returns how many entries went. The tenant's last
// administrator of its own is not removed (ErrLastAdministrator), and
// nothing of theirs is taken then.
func RemovePerson(ctx context.Context, s Store, tenant, sub string) (int, error) {
	user, err := User(sub)
	if err != nil {
		return 0, err
	}
	if !ValidName(tenant) {
		return 0, fmt.Errorf("%w: tenant %q", ErrInvalidID, tenant)
	}
	tuples, err := heldBy(ctx, s, tenant, user)
	if err != nil {
		return 0, err
	}
	if len(tuples) == 0 {
		return 0, nil
	}
	if err := keepsAnAdministrator(ctx, s, tenant, tuples); err != nil {
		return 0, err
	}
	if err := s.Write(ctx, nil, tuples); err != nil {
		return 0, fmt.Errorf("remove what %s holds in %s: %w", user, tenant, err)
	}
	return len(tuples), nil
}

// Administrators reads the accounts that administer tenant as its own: the
// ones given admin on it, and the holders of every position that holds
// admin on it, the positions that include such a position among them. The
// cluster's administrators, who act in a tenant that lets them, are not the
// tenant's own. Subjects, sorted.
func Administrators(ctx context.Context, s Store, tenant string) ([]string, error) {
	users, err := administrators(ctx, s, tenant, nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(users))
	for user := range users {
		out = append(out, strings.TrimPrefix(user, "user:"))
	}
	sort.Strings(out)
	return out, nil
}

// administrators is Administrators as the store names them, with the
// entries in without left out of the reading: who would administer the
// tenant once those are gone.
func administrators(ctx context.Context, s Store, tenant string, without []Tuple) (map[string]bool, error) {
	gone := map[string]bool{}
	for _, t := range without {
		gone[key(plain(t))] = true
	}
	on, err := s.Read(ctx, Tuple{Relation: RoleAdmin, Object: Tenant(tenant)})
	if err != nil {
		return nil, fmt.Errorf("read the administrators of %s: %w", tenant, err)
	}
	return holdersAmong(ctx, s, tenant, on, gone)
}

// holdersAmong is the accounts a list of entries on one object names: the
// ones named directly, and the holders of every position named, the
// positions that include such a position among them. Entries in gone are
// read as if they were not there.
func holdersAmong(ctx context.Context, s Store, tenant string, on []Tuple, gone map[string]bool) (map[string]bool, error) {
	users := map[string]bool{}
	var queue []string
	seen := map[string]bool{}
	for _, t := range on {
		p := plain(t)
		if gone[key(p)] {
			continue
		}
		object, relation, userset := strings.Cut(p.User, "#")
		switch {
		case !userset && strings.HasPrefix(p.User, "user:"):
			users[p.User] = true
		case userset && relation == "holder" && ofTenant(object, "position:", tenant) && !seen[object]:
			seen[object] = true
			queue = append(queue, object)
		}
	}
	for len(queue) > 0 {
		position := queue[0]
		queue = queue[1:]
		entries, err := s.Read(ctx, Tuple{Object: position})
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", position, err)
		}
		for _, t := range entries {
			p := plain(t)
			if gone[key(p)] {
				continue
			}
			switch {
			case p.Relation == "holder" && strings.HasPrefix(p.User, "user:") && !strings.Contains(p.User, "#"):
				users[p.User] = true
			case p.Relation == "included_in" && ofTenant(p.User, "position:", tenant) && !seen[p.User]:
				// Whoever holds the including position holds this one.
				seen[p.User] = true
				queue = append(queue, p.User)
			}
		}
	}
	return users, nil
}

// AppHolders reads who holds a role on one app of tenant, for an app that
// cannot read its people's roles from a token and is told them instead.
//
// Administrators are the accounts given admin on the app, directly or
// through a position, and for an add-on the administrators of its base.
// Users are everyone who may use the app: the accounts given user, the
// holders of each role its profile declares, and its administrators.
// Subjects, sorted.
func AppHolders(ctx context.Context, s Store, tenant, profile string) (administrators, users []string, err error) {
	if !ValidName(tenant) || !ValidName(profile) {
		return nil, nil, fmt.Errorf("%w: %q", ErrInvalidID, tenant+"/"+profile)
	}
	app := App(tenant, profile)
	collect := func(into map[string]bool, object, relation string) error {
		on, err := s.Read(ctx, Tuple{Relation: relation, Object: object})
		if err != nil {
			return fmt.Errorf("read %s on %s: %w", relation, object, err)
		}
		got, err := holdersAmong(ctx, s, tenant, on, nil)
		if err != nil {
			return err
		}
		for user := range got {
			into[user] = true
		}
		return nil
	}
	admins := map[string]bool{}
	if err := collect(admins, app, RoleAdmin); err != nil {
		return nil, nil, err
	}
	bases, err := s.Read(ctx, Tuple{Relation: "base", Object: app})
	if err != nil {
		return nil, nil, fmt.Errorf("read the base of %s: %w", app, err)
	}
	for _, t := range bases {
		if ofTenant(t.User, "app:", tenant) {
			if err := collect(admins, t.User, RoleAdmin); err != nil {
				return nil, nil, err
			}
		}
	}
	all := map[string]bool{}
	for user := range admins {
		all[user] = true
	}
	if err := collect(all, app, RoleUser); err != nil {
		return nil, nil, err
	}
	roles, err := declaredRolesOf(ctx, s.Read, tenant, app)
	if err != nil {
		return nil, nil, err
	}
	for _, role := range roles {
		if err := collect(all, role, "holder"); err != nil {
			return nil, nil, err
		}
	}
	return subjectsOf(admins), subjectsOf(all), nil
}

// PositionHolders reads the accounts that hold a position of tenant: the
// ones given it, and the holders of every position that includes it. The
// holders of user are the people of the tenant. Subjects, sorted.
func PositionHolders(ctx context.Context, s Store, tenant, position string) ([]string, error) {
	if !ValidName(tenant) || !ValidName(position) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidID, tenant+"/"+position)
	}
	users, err := holdersAmong(ctx, s, tenant, []Tuple{{User: Holders(Position(tenant, position))}}, nil)
	if err != nil {
		return nil, err
	}
	return subjectsOf(users), nil
}

func subjectsOf(users map[string]bool) []string {
	out := make([]string, 0, len(users))
	for user := range users {
		out = append(out, strings.TrimPrefix(user, "user:"))
	}
	sort.Strings(out)
	return out
}

// keepsAnAdministrator refuses taking entries that would leave the tenant
// with no administrator of its own. Entries that have nothing to do with
// administering it are not looked at twice: the tenant is read only when
// one of them could matter.
func keepsAnAdministrator(ctx context.Context, s Store, tenant string, taking []Tuple) error {
	matters := false
	for _, t := range taking {
		if t.Object == Tenant(tenant) && t.Relation == RoleAdmin {
			matters = true
		}
		if strings.HasPrefix(t.Object, "position:") {
			matters = true
		}
	}
	if !matters {
		return nil
	}
	before, err := administrators(ctx, s, tenant, nil)
	if err != nil {
		return err
	}
	if len(before) == 0 {
		// Nobody administers it as it is; taking something cannot make
		// that worse.
		return nil
	}
	after, err := administrators(ctx, s, tenant, taking)
	if err != nil {
		return err
	}
	if len(after) == 0 {
		return fmt.Errorf("%w: %s", ErrLastAdministrator, tenant)
	}
	return nil
}
