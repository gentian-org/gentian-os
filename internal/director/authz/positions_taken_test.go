/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"slices"
	"testing"
)

// The people a change of positions took something from: whoever held a
// position that lost a role or an inclusion, or is gone, and whoever holds a
// position that includes one of those.
func TestAChangeOfPositionsSaysFromWhomItTook(t *testing.T) {
	const tenant = "acme"
	sales, exec, chief, user, gone := Position(tenant, "sales"), Position(tenant, "executive"), Position(tenant, "chief"), Position(tenant, PositionUser), Position(tenant, "old")
	losses := func(deleted ...Tuple) *positionLosses {
		return &positionLosses{
			wanted: map[string]bool{sales: true, exec: true, chief: true, user: true},
			holders: map[string][]string{
				sales: {"user:seller", "user:team/claimed"},
				exec:  {"user:boss"},
				chief: {"user:top"},
				user:  {"user:everyone"},
				gone:  {"user:leaver"},
			},
			// executive includes sales; chief includes executive.
			includers: map[string][]string{sales: {exec}, exec: {chief}},
			deleted:   deleted,
		}
	}
	installed := func(object string) bool { return object != App(tenant, "removed") }

	for name, tc := range map[string]struct {
		deleted []Tuple
		want    []string
	}{
		"nothing changed: only the position no longer declared": {
			want: []string{"leaver"},
		},
		"a role taken from a position, and from what includes it": {
			deleted: []Tuple{{User: Holders(sales), Relation: RoleUser, Object: App(tenant, "crm")}},
			want:    []string{"boss", "leaver", "seller", "top"},
		},
		"a declared role taken": {
			deleted: []Tuple{{User: Holders(exec), Relation: "holder", Object: Role(tenant, "crm", "manager")}},
			want:    []string{"boss", "leaver", "top"},
		},
		"an inclusion taken": {
			deleted: []Tuple{{User: exec, Relation: "included_in", Object: sales}},
			want:    []string{"boss", "leaver", "top"},
		},
		"a role on an app no longer installed takes nothing": {
			deleted: []Tuple{{User: Holders(user), Relation: RoleUser, Object: App(tenant, "removed")}},
			want:    []string{"leaver"},
		},
		"a role on the tenant": {
			deleted: []Tuple{{User: Holders(user), Relation: RoleUser, Object: Tenant(tenant)}},
			want:    []string{"everyone", "leaver"},
		},
	} {
		got := losses(tc.deleted...).takenFrom(installed)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}

	// A position no longer declared and holding nobody still takes from
	// what included it.
	empty := &positionLosses{
		wanted:    map[string]bool{exec: true},
		holders:   map[string][]string{exec: {"user:boss"}},
		includers: map[string][]string{gone: {exec}},
		deleted:   []Tuple{{User: Tenant(tenant), Relation: "tenant", Object: gone}, {User: exec, Relation: "included_in", Object: gone}},
	}
	if got := empty.takenFrom(nil); !slices.Equal(got, []string{"boss"}) {
		t.Errorf("a gone position's includers: %v", got)
	}
}
