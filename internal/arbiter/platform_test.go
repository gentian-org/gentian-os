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
	"sort"
	"strings"
	"testing"
)

const teamModel = `model
  schema 1.1
type user
type group
  relations
    define member: [user]
type project
  relations
    define owner: [user, group#member]
    define viewer: [user, group#member, user:*] or owner
`

func mirroring(t *testing.T, tenant, component, key, realm string, tuples ...Tuple) Entry {
	t.Helper()
	e := entry(t, tenant, component, key, teamModel)
	e.Realm, e.MirrorGroups, e.Tuples = realm, true, tuples
	return e
}

func (h *harness) tuplesOf(name string) []string {
	_, st := h.fga.storeNamed(name)
	if st == nil {
		return nil
	}
	h.fga.mu.Lock()
	defer h.fga.mu.Unlock()
	var out []string
	for _, x := range st.tuples {
		out = append(out, x.User+" "+x.Relation+" "+x.Object)
	}
	sort.Strings(out)
	return out
}

// The platform's path admits the operator's ServiceAccount by its projected
// token and nobody else: not a component's key, not a component's own token
// for the same audience, not the operator's token for another audience.
func TestOnlyThePlatformUsesThePlatformPath(t *testing.T) {
	h := newHarness(t, mirroring(t, "acme", "ledger", "key-a", "acme"))
	body := MembershipsRequest{Tenant: "acme", Realm: "acme", Subject: "u1", Groups: []string{"staff"}}
	for token, want := range map[string]int{
		"":               http.StatusUnauthorized,
		"key-a":          http.StatusUnauthorized,
		"app-token":      http.StatusForbidden,
		"other-audience": http.StatusUnauthorized,
		"operator-token": http.StatusNoContent,
	} {
		if code, _ := h.call(t, http.MethodPut, "/v1/platform/memberships", token, body); code != want {
			t.Errorf("token %q: %d, want %d", token, code, want)
		}
	}
}

// A realm's memberships reach the stores of that tenant's components that
// mirror groups, as state: the person's groups become exactly the ones the
// event names, nested groups are not mirrored, and a deleted group loses
// its members. Another tenant's store and one that does not mirror are
// untouched.
func TestARealmsMembershipsReachItsTenantsMirroringStoresOnly(t *testing.T) {
	plain := entry(t, "acme", "notes", "key-n", teamModel)
	plain.Realm = "acme"
	h := newHarness(t,
		mirroring(t, "acme", "ledger", "key-a", "acme"),
		mirroring(t, "globex", "ledger", "key-g", "globex"),
		plain)
	put := func(subject string, groups ...string) {
		t.Helper()
		if code, body := h.call(t, http.MethodPut, "/v1/platform/memberships", "operator-token",
			MembershipsRequest{Tenant: "acme", Realm: "acme", Subject: subject, Groups: groups}); code != http.StatusNoContent {
			t.Fatalf("memberships: %d %v", code, body)
		}
	}
	put("u1", "/gentian:tenant:acme:members", "staff", "/staff/berlin")
	put("u2", "staff")
	want := []string{
		"user:acme/u1 member group:acme/gentian/tenant/acme/members",
		"user:acme/u1 member group:acme/staff",
		"user:acme/u2 member group:acme/staff",
	}
	if got := h.tuplesOf("acme/ledger"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("acme/ledger:\n%s", strings.Join(got, "\n"))
	}
	if got := h.tuplesOf("globex/ledger"); len(got) != 0 {
		t.Fatalf("another tenant's store got %v", got)
	}
	if got := h.tuplesOf("acme/notes"); len(got) != 0 {
		t.Fatalf("a store that does not mirror got %v", got)
	}
	// State: u1 is now in staff alone.
	put("u1", "staff")
	if code, _ := h.call(t, http.MethodPost, "/v1/platform/group-deleted", "operator-token",
		GroupDeletedRequest{Tenant: "acme", Realm: "acme", Group: "staff"}); code != http.StatusNoContent {
		t.Fatalf("group-deleted: %d", code)
	}
	if got := h.tuplesOf("acme/ledger"); len(got) != 0 {
		t.Fatalf("after the group went: %v", got)
	}
	// A realm that is not the store's tenant's reaches nothing.
	h.call(t, http.MethodPut, "/v1/platform/memberships", "operator-token",
		MembershipsRequest{Tenant: "acme", Realm: "globex", Subject: "u9", Groups: []string{"staff"}})
	if got := h.tuplesOf("acme/ledger"); len(got) != 0 {
		t.Fatalf("another realm's event reached acme: %v", got)
	}
}

// A component writes no tuple whose user or object is a group; it reads and
// asks about them as about any other.
func TestAComponentWritesNoGroupTuple(t *testing.T) {
	h := newHarness(t, mirroring(t, "acme", "ledger", "key-a", "acme"))
	for _, tuple := range []Tuple{
		{User: "user:acme/u1", Relation: "member", Object: "group:acme/staff"},
		{User: "group:acme/staff#member", Relation: "viewer", Object: "project:1"},
	} {
		for _, req := range []WriteRequest{{Writes: []Tuple{tuple}}, {Deletes: []Tuple{tuple}}} {
			code, body := h.call(t, http.MethodPost, "/v1/write", "key-a", req)
			if code != http.StatusForbidden || !strings.Contains(body["message"].(string), "platform") {
				t.Fatalf("%+v: %d %v", req, code, body)
			}
		}
	}
	if code, _ := h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{{User: "user:acme/u1", Relation: "owner", Object: "project:1"}}}); code != http.StatusOK {
		t.Fatalf("an ordinary tuple: %d", code)
	}
	h.call(t, http.MethodPut, "/v1/platform/memberships", "operator-token", MembershipsRequest{Tenant: "acme", Realm: "acme", Subject: "u1", Groups: []string{"staff"}})
	if _, body := h.call(t, http.MethodPost, "/v1/check", "key-a", CheckRequest{User: "user:acme/u1", Relation: "member", Object: "group:acme/staff"}); body["allowed"] != true {
		t.Fatalf("a check of a mirrored membership: %v", body)
	}
}

// Starting tuples are the profile's: written, and taken away when the
// profile no longer names them, while what the component wrote is left.
func TestStartingTuplesFollowTheProfileAndLeaveTheComponentsOwn(t *testing.T) {
	seed := Tuple{User: "user:*", Relation: "viewer", Object: "project:public"}
	first := mirroring(t, "acme", "ledger", "key-a", "acme", seed)
	h := newHarness(t, first)
	h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{{User: "user:acme/u1", Relation: "owner", Object: "project:1"}}})
	if got := h.tuplesOf("acme/ledger"); len(got) != 2 {
		t.Fatalf("tuples = %v", got)
	}
	// The profile's next build names another starting tuple: the first
	// goes, the component's own stays.
	other := Tuple{User: "user:acme/u2", Relation: "owner", Object: "project:2"}
	next := first
	next.Tuples = []Tuple{other}
	h.server.SetTable(&Table{Stores: []Entry{next}})
	h.call(t, http.MethodGet, "/v1/model", "key-a", nil)
	want := []string{"user:acme/u1 owner project:1", "user:acme/u2 owner project:2"}
	if got := h.tuplesOf("acme/ledger"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tuples = %v, want %v", got, want)
	}
	if got := h.rec.ids["acme.ledger"].Written; len(got) != 1 || got[0] != other {
		t.Fatalf("recorded as written: %v", got)
	}
}

// What a store holds goes into a bundle and comes back, into a fresh store
// that holds that and nothing else; a purge deletes the store and its record.
func TestAStoreIsExportedRestoredAndPurged(t *testing.T) {
	h := newHarness(t, mirroring(t, "acme", "ledger", "key-a", "acme"))
	h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{{User: "user:acme/u1", Relation: "owner", Object: "project:1"}}})
	h.call(t, http.MethodPut, "/v1/platform/memberships", "operator-token", MembershipsRequest{Tenant: "acme", Realm: "acme", Subject: "u1", Groups: []string{"staff"}})
	code, body := h.call(t, http.MethodGet, "/v1/platform/stores/acme/ledger", "operator-token", nil)
	if code != http.StatusOK || len(body["tuples"].([]any)) != 2 || body["model"] == nil {
		t.Fatalf("export: %d %v", code, body)
	}
	if _, has := body["model"].(map[string]any)["id"]; has {
		t.Fatal("the export carries the server's model id")
	}
	before, _ := h.fga.storeNamed("acme/ledger")
	h.call(t, http.MethodPost, "/v1/write", "key-a", WriteRequest{Writes: []Tuple{{User: "user:acme/u3", Relation: "owner", Object: "project:3"}}})
	if code, b := h.call(t, http.MethodPut, "/v1/platform/stores/acme/ledger", "operator-token", body); code != http.StatusNoContent {
		t.Fatalf("restore: %d %v", code, b)
	}
	after, _ := h.fga.storeNamed("acme/ledger")
	if after == before || len(h.tuplesOf("acme/ledger")) != 2 {
		t.Fatalf("restored into %s from %s: %v", after, before, h.tuplesOf("acme/ledger"))
	}
	if code, _ := h.call(t, http.MethodPost, "/v1/check", "key-a", CheckRequest{User: "user:acme/u1", Relation: "owner", Object: "project:1"}); code != http.StatusOK {
		t.Fatalf("the restored store does not answer: %d", code)
	}
	if code, _ := h.call(t, http.MethodDelete, "/v1/platform/stores/acme/ledger", "operator-token", nil); code != http.StatusNoContent {
		t.Fatalf("purge: %d", code)
	}
	if id, _ := h.fga.storeNamed("acme/ledger"); id != "" {
		t.Fatal("the purged store is still there")
	}
	if _, ok := h.rec.ids["acme.ledger"]; ok {
		t.Fatal("the purged store is still recorded")
	}
	if code, _ := h.call(t, http.MethodGet, "/v1/platform/stores/acme/ledger", "operator-token", nil); code != http.StatusNotFound {
		t.Fatalf("export of a purged store: %d", code)
	}
}

// The naming of people and groups, and what is refused.
func TestPeopleAndGroupsAreNamedByTheirRealm(t *testing.T) {
	if got, _ := UserObject("acme", "f:google:123"); got != "user:acme/f/google/123" {
		t.Errorf("user = %q", got)
	}
	if got, _ := GroupObject("acme", "/gentian:tenant:acme:members"); got != "group:acme/gentian/tenant/acme/members" {
		t.Errorf("group = %q", got)
	}
	for _, bad := range []string{"/staff/berlin", "two words", "a#b", ""} {
		if _, err := GroupObject("acme", bad); err == nil {
			t.Errorf("group %q was named", bad)
		}
	}
	if _, err := UserObject("Not A Realm", "u"); err == nil {
		t.Error("a realm that is no name was taken")
	}
}

// Starting tuples are held to the compiled model when the profile is
// admitted, and so is a store that mirrors groups.
func TestStartingTuplesAreHeldToTheModel(t *testing.T) {
	model, err := Compile(teamModel)
	if err != nil {
		t.Fatal(err)
	}
	good := []Tuple{
		{User: "user:*", Relation: "viewer", Object: "project:public"},
		{User: "group:acme/staff#member", Relation: "owner", Object: "project:1"},
		{User: "user:acme/u1", Relation: "owner", Object: "project:1"},
	}
	if err := ValidateStartingTuples(model, good, true); err != nil {
		t.Fatalf("good tuples: %v", err)
	}
	for _, bad := range []Tuple{
		{User: "user:*", Relation: "owner", Object: "project:1"},
		{User: "user:acme/u1", Relation: "editor", Object: "project:1"},
		{User: "user:acme/u1", Relation: "owner", Object: "doc:1"},
		{User: "user:acme/u1", Relation: "member", Object: "group:acme/staff"},
		{User: "group:acme/staff", Relation: "owner", Object: "project:1"},
	} {
		if err := ValidateStartingTuples(model, []Tuple{bad}, true); err == nil {
			t.Errorf("%+v was admitted", bad)
		}
	}
	if err := ValidateStartingTuples(model, []Tuple{good[0], good[0]}, false); err == nil {
		t.Error("a tuple named twice was admitted")
	}
	noGroups, _ := Compile(docModel)
	if err := ValidateStartingTuples(noGroups, nil, true); err == nil {
		t.Error("a model with no group type was let mirror groups")
	}
}
