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

// RoleHolding is one person's roles on one app: user, admin and the roles
// its profile declares, by the subject of the person's tokens.
type RoleHolding struct {
	Subject string   `json:"subject"`
	Roles   []string `json:"roles"`
}

// AppRoleHoldings answers, for each app of tenant named, who holds which of
// its roles, as a check on the store would: through the positions a person
// holds and what they include, through declared roles and what they
// include, through a base's administrators, and with the app's
// administrator holding user and every role it declares. It is what an app
// that takes its people's roles into its own store is handed (the arbiter's
// platform path): roles, never the positions they come through.
//
// Keyed by profile; each list sorted by subject, with nobody who holds
// nothing. A profile the tenant has not installed holds nobody.
func AppRoleHoldings(ctx context.Context, s Store, tenant string, profiles []string) (map[string][]RoleHolding, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidID, tenant)
	}
	g, err := readTenantGraph(ctx, s.Read, tenant)
	if err != nil {
		return nil, err
	}

	// held[user] is everything a person holds: what they were given, and
	// what each position they were given holds.
	held := map[string]map[grant]bool{}
	add := func(user string, from map[grant]bool) {
		if held[user] == nil {
			held[user] = map[grant]bool{}
		}
		for gr := range from {
			held[user][gr] = true
		}
	}
	for subject := range g.given {
		if isPerson(subject) {
			add(subject, g.effective(subject))
		}
	}
	for _, pos := range g.positions {
		holders, err := s.Read(ctx, Tuple{Relation: "holder", Object: pos})
		if err != nil {
			return nil, fmt.Errorf("read the holders of %s: %w", pos, err)
		}
		var of map[grant]bool
		for _, t := range holders {
			if !isPerson(t.User) {
				continue
			}
			if of == nil {
				of = g.effective(Holders(pos))
			}
			add(t.User, of)
		}
	}

	out := map[string][]RoleHolding{}
	for _, profile := range profiles {
		if !ValidName(profile) {
			return nil, fmt.Errorf("%w: profile %q", ErrInvalidID, profile)
		}
		app := App(tenant, profile)
		var declared []string
		for role, a := range g.roleApp {
			if a == app {
				declared = append(declared, roleNameOf(role))
			}
		}
		prefix := "role:" + tenant + "/" + profile + "/"
		holdings := []RoleHolding{}
		for user, grants := range held {
			roles := map[string]bool{}
			if grants[grant{RoleAdmin, app}] {
				roles[RoleAdmin], roles[RoleUser] = true, true
				for _, name := range declared {
					roles[name] = true
				}
			}
			if grants[grant{RoleUser, app}] {
				roles[RoleUser] = true
			}
			for gr := range grants {
				if gr.relation == "holder" && strings.HasPrefix(gr.object, prefix) {
					roles[roleNameOf(gr.object)] = true
				}
			}
			if len(roles) == 0 {
				continue
			}
			names := make([]string, 0, len(roles))
			for name := range roles {
				names = append(names, name)
			}
			sort.Strings(names)
			holdings = append(holdings, RoleHolding{Subject: subjectOf(user), Roles: names})
		}
		sort.Slice(holdings, func(i, j int) bool { return holdings[i].Subject < holdings[j].Subject })
		out[profile] = holdings
	}
	return out, nil
}

// isPerson reports whether a store user is one person (user:<sub>, as User
// writes it) and not a set of holders (position:...#holder). Roles on apps,
// declared roles and positions are given to persons by User alone.
func isPerson(user string) bool {
	return strings.HasPrefix(user, "user:") && !strings.Contains(user, "#")
}

// subjectOf is the token subject a person's store user was made from: User
// turned ':' into '/', and nothing else.
func subjectOf(user string) string {
	return strings.ReplaceAll(strings.TrimPrefix(user, "user:"), "/", ":")
}
