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

// What follows from what was given.
//
// The model keeps a check to one step on each thing: it does not walk a
// position's inclusions, an app's declared roles, its add-ons or its base.
// What those would have given is written here instead, as derived_* entries,
// by the operator alone and never given by anybody: whenever positions or
// apps are projected for a tenant, and whenever a person is given or loses a
// role directly. A derived entry is exactly what the walk would have found,
// so taking the thing it follows from takes it too.
//
// The rules, applied until nothing more follows, for each position's holders
// and each person holding a role directly:
//   - a position holds what every position it includes holds;
//   - a role holds every role it includes (role included_in);
//   - a declared role is held on its app, so it lets the holder use the app;
//   - the admin of a base administers its add-ons;
//   - any role on an add-on lets the holder use its base.
//
// What is given stays in the given relations (user, admin, holder), so the
// console tells a role given to a position from one it inherits.

// derivedPrefix starts every relation the operator derives. Nothing else is
// written under it, and no reader shows it as a role somebody was given.
const derivedPrefix = "derived_"

// IsDerived reports whether a relation is one the operator derives.
func IsDerived(relation string) bool { return strings.HasPrefix(relation, derivedPrefix) }

// grant is one given or derived thing a subject holds: a relation of the
// model's given ones (user, admin, holder) on an object.
type grant struct {
	relation, object string
}

// tenantGraph is what derivation reads of one tenant: who holds what given,
// and the structure the rules follow.
type tenantGraph struct {
	tenant string
	// given[subject] are the given grants of a position's holders
	// (position:<t>/<p>#holder) or a person (user:...).
	given map[string]map[grant]bool
	// derived are the derived entries the store holds now, by key.
	derived map[string]Tuple
	// includes[position] are the positions it includes.
	includes map[string][]string
	// roleIncludes[role] are the roles it includes.
	roleIncludes map[string][]string
	// roleApp[role] is the app it is declared on.
	roleApp map[string]string
	// addons[app] are its add-ons; base[app] is an add-on's base.
	addons map[string][]string
	base   map[string]string
	// positions are the tenant's positions.
	positions []string
}

// readTenantGraph reads one tenant's things: the tenant, its apps and their
// declared roles, and its positions.
func readTenantGraph(ctx context.Context, read readFunc, tenant string) (*tenantGraph, error) {
	g := &tenantGraph{
		tenant:       tenant,
		given:        map[string]map[grant]bool{},
		derived:      map[string]Tuple{},
		includes:     map[string][]string{},
		roleIncludes: map[string][]string{},
		roleApp:      map[string]string{},
		addons:       map[string][]string{},
		base:         map[string]string{},
	}
	give := func(subject, relation, object string) {
		if g.given[subject] == nil {
			g.given[subject] = map[grant]bool{}
		}
		g.given[subject][grant{relation, object}] = true
	}
	holderSubject := func(user string) bool {
		if strings.HasPrefix(user, "user:") && !strings.Contains(user, "#") {
			return true
		}
		object, rel, ok := strings.Cut(user, "#")
		return ok && rel == "holder" && ofTenant(object, "position:", tenant)
	}
	// on reads every entry on one object and sorts it into the graph.
	on := func(object string) ([]Tuple, error) {
		tuples, err := read(ctx, Tuple{Object: object})
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", object, err)
		}
		for _, t := range tuples {
			p := plain(t)
			switch {
			case IsDerived(p.Relation):
				g.derived[key(p)] = p
			case holderSubject(p.User) && isGivenRole(object, p.Relation):
				give(p.User, p.Relation, object)
			}
		}
		return tuples, nil
	}

	if _, err := on(Tenant(tenant)); err != nil {
		return nil, err
	}

	apps, err := read(ctx, Tuple{User: Tenant(tenant), Relation: "tenant", Object: "app:"})
	if err != nil {
		return nil, fmt.Errorf("read apps of tenant %s: %w", tenant, err)
	}
	var roles []string
	for _, a := range apps {
		if !ofTenant(a.Object, "app:", tenant) {
			continue
		}
		tuples, err := on(a.Object)
		if err != nil {
			return nil, err
		}
		for _, t := range tuples {
			p := plain(t)
			switch {
			case p.Relation == "base" && ofTenant(p.User, "app:", tenant):
				g.base[a.Object] = p.User
				g.addons[p.User] = append(g.addons[p.User], a.Object)
			case p.Relation == "declared" && ofTenant(p.User, "role:", tenant):
				roles = append(roles, p.User)
			}
		}
	}
	sort.Strings(roles)
	for _, role := range roles {
		tuples, err := on(role)
		if err != nil {
			return nil, err
		}
		for _, t := range tuples {
			p := plain(t)
			switch {
			case p.Relation == "app" && ofTenant(p.User, "app:", tenant):
				g.roleApp[role] = p.User
			case p.Relation == "included_in" && ofTenant(p.User, "role:", tenant):
				// p.User included_in role: whoever holds p.User holds role.
				g.roleIncludes[p.User] = append(g.roleIncludes[p.User], role)
			}
		}
	}

	positions, err := read(ctx, Tuple{User: Tenant(tenant), Relation: "tenant", Object: "position:"})
	if err != nil {
		return nil, fmt.Errorf("read positions of tenant %s: %w", tenant, err)
	}
	for _, pos := range positions {
		if !ofTenant(pos.Object, "position:", tenant) {
			continue
		}
		g.positions = append(g.positions, pos.Object)
		tuples, err := read(ctx, Tuple{Relation: "included_in", Object: pos.Object})
		if err != nil {
			return nil, fmt.Errorf("read what includes %s: %w", pos.Object, err)
		}
		for _, t := range tuples {
			p := plain(t)
			if ofTenant(p.User, "position:", tenant) {
				// p.User included_in pos: p.User includes pos.
				g.includes[p.User] = append(g.includes[p.User], pos.Object)
			}
		}
	}
	sort.Strings(g.positions)
	return g, nil
}

// isGivenRole reports whether a relation on an object is a role somebody is
// given: user and admin on a tenant or an app, holder of a declared role.
func isGivenRole(object, relation string) bool {
	switch {
	case strings.HasPrefix(object, "tenant:"), strings.HasPrefix(object, "app:"):
		return relation == RoleUser || relation == RoleAdmin
	case strings.HasPrefix(object, "role:"):
		return relation == "holder"
	}
	return false
}

// effective is everything a subject holds: what it was given, what the
// positions it includes hold (for a position), and what follows by the
// rules.
func (g *tenantGraph) effective(subject string) map[grant]bool {
	out := map[grant]bool{}
	for gr := range g.given[subject] {
		out[gr] = true
	}
	// A position holds what every position it includes holds, transitively.
	if object, _, ok := strings.Cut(subject, "#"); ok {
		seen := map[string]bool{object: true}
		queue := append([]string{}, g.includes[object]...)
		for len(queue) > 0 {
			included := queue[0]
			queue = queue[1:]
			if seen[included] {
				continue
			}
			seen[included] = true
			for gr := range g.given[Holders(included)] {
				out[gr] = true
			}
			queue = append(queue, g.includes[included]...)
		}
	}
	for changed := true; changed; {
		changed = false
		add := func(gr grant) {
			if !out[gr] {
				out[gr] = true
				changed = true
			}
		}
		for gr := range out {
			switch {
			case strings.HasPrefix(gr.object, "role:"):
				for _, inc := range g.roleIncludes[gr.object] {
					add(grant{"holder", inc})
				}
				if app := g.roleApp[gr.object]; app != "" {
					add(grant{RoleUser, app})
				}
			case strings.HasPrefix(gr.object, "app:"):
				if gr.relation == RoleAdmin {
					for _, addon := range g.addons[gr.object] {
						add(grant{RoleAdmin, addon})
					}
				}
				if b := g.base[gr.object]; b != "" {
					add(grant{RoleUser, b})
				}
			}
		}
	}
	return out
}

// derivedRelation is the derived relation a given one is written under.
func derivedRelation(relation string) string { return derivedPrefix + relation }

// want is the derived entries a subject should have: what it holds that it
// was not given, less what the model already gives in one step on the same
// thing (user where admin is held).
func (g *tenantGraph) want(subject string) map[string]Tuple {
	out := map[string]Tuple{}
	held := g.effective(subject)
	for gr := range held {
		if g.given[subject][gr] {
			continue
		}
		if gr.relation == RoleUser && (held[grant{RoleAdmin, gr.object}]) {
			continue
		}
		t := Tuple{User: subject, Relation: derivedRelation(gr.relation), Object: gr.object}
		out[key(t)] = t
	}
	return out
}

// subjects are everyone derivation is for in the tenant: every position's
// holders, and every person who was given a role directly.
func (g *tenantGraph) subjects() []string {
	set := map[string]bool{}
	for _, p := range g.positions {
		set[Holders(p)] = true
	}
	for s := range g.given {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// diff is the writes and deletes that make the store's derived entries for
// the subjects given exactly what they should be.
func (g *tenantGraph) diff(subjects []string) (writes, deletes []Tuple) {
	wanted := map[string]Tuple{}
	for _, s := range subjects {
		for k, t := range g.want(s) {
			wanted[k] = t
		}
	}
	scope := map[string]bool{}
	for _, s := range subjects {
		scope[s] = true
	}
	for k, t := range wanted {
		if _, ok := g.derived[k]; !ok {
			writes = append(writes, t)
		}
	}
	for k, t := range g.derived {
		if !scope[t.User] {
			continue
		}
		if _, ok := wanted[k]; !ok {
			deletes = append(deletes, t)
		}
	}
	sort.Slice(writes, func(i, j int) bool { return key(writes[i]) < key(writes[j]) })
	sort.Slice(deletes, func(i, j int) bool { return key(deletes[i]) < key(deletes[j]) })
	return writes, deletes
}

// ReconcileDerived makes the store's derived entries for each tenant given
// exactly what follows from what is given there: for every position, and
// every person holding a role directly. A derived entry of a subject that no
// longer holds anything is removed.
func ReconcileDerived(ctx context.Context, s Store, tenants []string) error {
	for _, tenant := range tenants {
		if !ValidName(tenant) {
			return fmt.Errorf("%w: tenant %q", ErrInvalidID, tenant)
		}
		g, err := readTenantGraph(ctx, s.Read, tenant)
		if err != nil {
			return err
		}
		subjects := g.subjects()
		// Subjects that hold derived entries but nothing given any more.
		have := map[string]bool{}
		for _, s := range subjects {
			have[s] = true
		}
		for _, t := range g.derived {
			if !have[t.User] {
				subjects = append(subjects, t.User)
				have[t.User] = true
			}
		}
		writes, deletes := g.diff(subjects)
		if len(writes)+len(deletes) == 0 {
			continue
		}
		if err := s.Write(ctx, writes, deletes); err != nil {
			return fmt.Errorf("write what follows in tenant %s: %w", tenant, err)
		}
	}
	return nil
}

// deriveFor makes the derived entries of one person in tenant exactly what
// follows from the roles they were given directly. A position they hold
// needs nothing: the position carries its own.
func deriveFor(ctx context.Context, s Store, tenant, user string) error {
	g, err := readTenantGraph(ctx, s.Read, tenant)
	if err != nil {
		return err
	}
	writes, deletes := g.diff([]string{user})
	if len(writes)+len(deletes) == 0 {
		return nil
	}
	if err := s.Write(ctx, writes, deletes); err != nil {
		return fmt.Errorf("write what follows for %s in tenant %s: %w", user, tenant, err)
	}
	return nil
}
