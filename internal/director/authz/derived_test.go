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
	"sort"
	"strings"
	"testing"
)

// derivedOf lists the derived entries the store holds, as keys, sorted.
func derivedOf(s *writable) []string {
	var out []string
	for t := range s.memoryStore {
		if IsDerived(t.Relation) {
			out = append(out, key(t))
		}
	}
	sort.Strings(out)
	return out
}

// What follows for the positions: Chief holds what Deputy holds through the
// inclusion, App administrator administers the add-on of the shop it
// administers. Nothing else follows, and asked again nothing is written.
func TestTheProjectionWritesWhatFollowsForEachPosition(t *testing.T) {
	ctx := context.Background()
	s := tenantStore(t, "demo")
	if err := ReconcileDerived(ctx, s, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"position:demo/app-administrator#holder|derived_admin|app:demo/shop-sales",
		"position:demo/chief#holder|derived_admin|tenant:demo",
	}
	if got := derivedOf(s); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("derived:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	writes := s.writes
	if err := ReconcileDerived(ctx, s, []string{"demo"}); err != nil || s.writes != writes {
		t.Fatalf("asked again: err %v, %d more writes", err, s.writes-writes)
	}

	// Chief no longer includes Deputy: what it held through it goes.
	delete(s.memoryStore, Tuple{User: "position:demo/chief", Relation: "included_in", Object: "position:demo/deputy"})
	if err := ReconcileDerived(ctx, s, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
	if got := derivedOf(s); len(got) != 1 || !strings.HasPrefix(got[0], "position:demo/app-administrator#holder") {
		t.Fatalf("after the inclusion went: %v", got)
	}
}

// A role given to a person directly brings what follows from it, and taking
// it takes that too: a declared role on an add-on lets them use the add-on
// and its base.
func TestARoleGivenDirectlyBringsWhatFollowsAndTakesItAway(t *testing.T) {
	ctx := context.Background()
	s := tenantStore(t, "demo")
	if _, err := Give(ctx, s, "demo", "anna", Assignment{App: "shop-sales", Role: "manager"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"user:anna|derived_user|app:demo/shop",
		"user:anna|derived_user|app:demo/shop-sales",
	}
	if got := derivedOf(s); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("derived after the give:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := Take(ctx, s, "demo", "anna", Assignment{App: "shop-sales", Role: "manager"}); err != nil {
		t.Fatal(err)
	}
	if got := derivedOf(s); len(got) != 0 {
		t.Fatalf("derived after the take: %v", got)
	}

	// A position given derives nothing for the person: the position carries
	// its own.
	if _, err := Give(ctx, s, "demo", "anna", Assignment{Position: "chief"}); err != nil {
		t.Fatal(err)
	}
	if got := derivedOf(s); len(got) != 0 {
		t.Fatalf("derived after giving a position: %v", got)
	}
}

// Leaving takes what followed from the roles the person held directly.
func TestRemovingAPersonTakesWhatFollowedForThem(t *testing.T) {
	ctx := context.Background()
	s := tenantStore(t, "demo")
	if _, err := Give(ctx, s, "demo", "anna", Assignment{App: "shop", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if got := derivedOf(s); len(got) != 1 || got[0] != "user:anna|derived_admin|app:demo/shop-sales" {
		t.Fatalf("derived for the base's admin: %v", got)
	}
	if _, err := RemovePerson(ctx, s, "demo", "anna"); err != nil {
		t.Fatal(err)
	}
	if got := derivedOf(s); len(got) != 0 {
		t.Fatalf("derived after removal: %v", got)
	}
}

// A role that includes another, and an inclusion chain of positions, follow
// to the end.
func TestWhatFollowsFollowsToTheEnd(t *testing.T) {
	g := &tenantGraph{
		tenant: "demo",
		given: map[string]map[grant]bool{
			"position:demo/a#holder": {{"holder", "role:demo/shop-sales/manager"}: true},
		},
		derived:      map[string]Tuple{},
		includes:     map[string][]string{"position:demo/c": {"position:demo/b"}, "position:demo/b": {"position:demo/a"}},
		roleIncludes: map[string][]string{"role:demo/shop-sales/manager": {"role:demo/shop-sales/clerk"}},
		roleApp: map[string]string{
			"role:demo/shop-sales/manager": "app:demo/shop-sales",
			"role:demo/shop-sales/clerk":   "app:demo/shop-sales",
		},
		addons:    map[string][]string{"app:demo/shop": {"app:demo/shop-sales"}},
		base:      map[string]string{"app:demo/shop-sales": "app:demo/shop"},
		positions: []string{"position:demo/a", "position:demo/b", "position:demo/c"},
	}
	var got []string
	for k := range g.want("position:demo/c#holder") {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{
		"position:demo/c#holder|derived_holder|role:demo/shop-sales/clerk",
		"position:demo/c#holder|derived_holder|role:demo/shop-sales/manager",
		"position:demo/c#holder|derived_user|app:demo/shop",
		"position:demo/c#holder|derived_user|app:demo/shop-sales",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("derived for c:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A derived relation is nobody's role: the views do not show it.
func TestADerivedRelationIsNotARole(t *testing.T) {
	g, err := parseModel(modelV1)
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"tenant", "app", "role"} {
		for name := range g.role[typ] {
			if IsDerived(name) {
				t.Errorf("%s#%s is shown as a role", typ, name)
			}
		}
	}
}
