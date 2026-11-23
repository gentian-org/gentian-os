/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

const rolesModel = `model
  schema 1.1
type user
type role
  relations
    define holder: [user]
type shop
  relations
    define seller: [user, role#holder]
`

func handed(t *testing.T, tenant, component, key, realm string, tuples ...Tuple) Entry {
	t.Helper()
	e := entry(t, tenant, component, key, rolesModel)
	e.Realm, e.HandRoles, e.Tuples = realm, true, tuples
	return e
}

func roleTuplesOf(h *harness, name string) []string {
	var out []string
	for _, t := range h.tuplesOf(name) {
		if strings.Contains(t, " holder role:") {
			out = append(out, t)
		}
	}
	return out
}

// The platform states every person's roles on the app; the store holds
// exactly those, named by the store's realm, and what the component wrote
// itself is left. Stated again with someone gone, their roles go.
func TestAStoreHoldsTheRolesThePlatformHandsIt(t *testing.T) {
	seed := Tuple{User: "role:sales#holder", Relation: "seller", Object: "shop:main"}
	h := newHarness(t, handed(t, "acme", "ledger", "key-a", "acme", seed))
	h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{{User: "user:acme/u9", Relation: "seller", Object: "shop:side"}}})

	put := func(holdings ...authz.RoleHolding) {
		t.Helper()
		if holdings == nil {
			holdings = []authz.RoleHolding{}
		}
		if code, body := h.call(t, http.MethodPut, "/v1/platform/roles/acme/ledger", "operator-token", RolesRequest{Holdings: holdings}); code != http.StatusNoContent {
			t.Fatalf("hand over: %d %v", code, body)
		}
	}
	put(authz.RoleHolding{Subject: "u1", Roles: []string{"sales", "user"}},
		authz.RoleHolding{Subject: "f:google:7", Roles: []string{"admin"}})
	want := []string{
		"user:acme/f/google/7 holder role:admin",
		"user:acme/u1 holder role:sales",
		"user:acme/u1 holder role:user",
	}
	if got := roleTuplesOf(h, "acme/ledger"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if _, body := h.call(t, http.MethodPost, "/v1/check", "key-a", CheckRequest{User: "user:acme/u1", Relation: "holder", Object: "role:sales"}); body["allowed"] != true {
		t.Fatalf("a check of a handed role: %v", body)
	}

	put(authz.RoleHolding{Subject: "u1", Roles: []string{"user"}})
	if got := roleTuplesOf(h, "acme/ledger"); strings.Join(got, "|") != "user:acme/u1 holder role:user" {
		t.Fatalf("after the second hand-over: %v", got)
	}
	all := strings.Join(h.tuplesOf("acme/ledger"), "|")
	if !strings.Contains(all, "role:sales#holder seller shop:main") || !strings.Contains(all, "user:acme/u9 seller shop:side") {
		t.Fatalf("the starting and the component's tuples went: %v", all)
	}
	put()
	if got := roleTuplesOf(h, "acme/ledger"); len(got) != 0 {
		t.Fatalf("after nobody holds anything: %v", got)
	}
}

// Only a store the table hands roles is handed any, and only with names a
// store can hold.
func TestRolesReachOnlyAStoreHandedThem(t *testing.T) {
	h := newHarness(t, handed(t, "acme", "ledger", "key-a", "acme"), mirroring(t, "acme", "wiki", "key-w", "acme"))
	for path, want := range map[string]int{
		"/v1/platform/roles/acme/wiki":     http.StatusNotFound,
		"/v1/platform/roles/globex/ledger": http.StatusNotFound,
		"/v1/platform/roles/acme/ledger":   http.StatusNoContent,
	} {
		if code, _ := h.call(t, http.MethodPut, path, "operator-token", RolesRequest{Holdings: []authz.RoleHolding{}}); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	if code, _ := h.call(t, http.MethodPut, "/v1/platform/roles/acme/ledger", "key-a", RolesRequest{}); code != http.StatusUnauthorized {
		t.Errorf("a component's key: %d", code)
	}
	for _, bad := range []authz.RoleHolding{
		{Subject: "u1", Roles: []string{"Sales"}},
		{Subject: "u1", Roles: []string{"a#b"}},
		{Subject: "a/b", Roles: []string{"user"}},
	} {
		if code, _ := h.call(t, http.MethodPut, "/v1/platform/roles/acme/ledger", "operator-token", RolesRequest{Holdings: []authz.RoleHolding{bad}}); code != http.StatusBadRequest {
			t.Errorf("%+v: %d", bad, code)
		}
	}
}

// In a store handed roles the component writes no tuple about a role; its
// own objects it gives to a role's holders as to anybody.
func TestAComponentWritesNoTupleAboutAHandedRole(t *testing.T) {
	h := newHarness(t, handed(t, "acme", "ledger", "key-a", "acme"))
	for _, req := range []WriteRequest{
		{Writes: []Tuple{{User: "user:acme/u1", Relation: "holder", Object: "role:sales"}}},
		{Deletes: []Tuple{{User: "user:acme/u1", Relation: "holder", Object: "role:sales"}}},
	} {
		if code, body := h.call(t, http.MethodPost, "/v1/write", "key-a", req); code != http.StatusForbidden {
			t.Fatalf("%+v: %d %v", req, code, body)
		}
	}
	if code, body := h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{{User: "role:sales#holder", Relation: "seller", Object: "shop:1"}}}); code != http.StatusOK {
		t.Fatalf("its own object for a role's holders: %d %v", code, body)
	}
}

// A store that is no longer handed roles, or no longer mirrors groups, loses
// what the platform kept there for it.
func TestAStoreThatStopsBeingHandedRolesLosesThem(t *testing.T) {
	first := handed(t, "acme", "ledger", "key-a", "acme")
	h := newHarness(t, first)
	h.call(t, http.MethodPut, "/v1/platform/roles/acme/ledger", "operator-token",
		RolesRequest{Holdings: []authz.RoleHolding{{Subject: "u1", Roles: []string{"sales"}}}})
	h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{{User: "user:acme/u2", Relation: "seller", Object: "shop:1"}}})
	next := first
	next.HandRoles = false
	h.server.SetTable(&Table{Stores: []Entry{next}})
	h.call(t, http.MethodGet, "/v1/model", "key-a", nil)
	if got := h.tuplesOf("acme/ledger"); strings.Join(got, "|") != "user:acme/u2 seller shop:1" {
		t.Fatalf("tuples = %v", got)
	}
	if h.rec.ids["acme.ledger"].HandedRoles {
		t.Fatal("still recorded as handed roles")
	}
}

// A profile handed roles has a model that takes them, and names no role as
// the object of a starting tuple.
func TestAStoreHandedRolesHasAModelForThem(t *testing.T) {
	model, err := Compile(rolesModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateStartingTuples(model, []Tuple{{User: "role:sales#holder", Relation: "seller", Object: "shop:1"}}, false, true); err != nil {
		t.Fatalf("a good one: %v", err)
	}
	if err := ValidateStartingTuples(model, []Tuple{{User: "user:acme/u1", Relation: "holder", Object: "role:sales"}}, false, true); err == nil {
		t.Error("a starting tuple about a role was admitted")
	}
	noRoles, _ := Compile(teamModel)
	if err := ValidateStartingTuples(noRoles, nil, false, true); err == nil {
		t.Error("a model with no role type was handed roles")
	}
	if _, err := ParseTable([]byte(`{"stores":[{"tenant":"a","component":"b","keyHash":"` + strings.Repeat("0", 64) + `","model":{},"handRoles":true}]}`)); err == nil {
		t.Error("a table handing roles without a realm was read")
	}
}
