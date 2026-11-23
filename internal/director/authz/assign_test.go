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
	"reflect"
	"strings"
	"testing"
)

// writable is the store as giving and taking use it, in memory.
type writable struct {
	memoryStore
	writes, deletes int
}

func (w *writable) Write(_ context.Context, writes, deletes []Tuple) error {
	for _, t := range deletes {
		if !w.memoryStore[t] {
			return errors.New("the store does not hold " + key(t))
		}
		delete(w.memoryStore, t)
		w.deletes++
	}
	for _, t := range writes {
		if w.memoryStore[t] {
			return errors.New("the store already holds " + key(t))
		}
		w.memoryStore[t] = true
		w.writes++
	}
	return nil
}

// tenantStore is a tenant as the projections leave it: attached, with the
// three positions every tenant has, a shop with a sales add-on that declares
// a role, and two positions of its own, one including the other.
func tenantStore(t *testing.T, tenant string) *writable {
	t.Helper()
	apps := []InstalledApp{{Profile: "wiki", ForEveryone: true}, {Profile: "shop", Addons: []InstalledApp{
		{Profile: "shop-sales", Roles: []DeclaredRole{{Name: "manager"}}},
	}}}
	store := projection(t, "c1", tenant, apps)
	more, err := positionTuples(tenant, []PositionSpec{
		{Name: "deputy", Roles: []RoleGrant{{Role: RoleAdmin}}},
		{Name: "chief", Includes: []string{"deputy"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tuple := range more {
		store[tuple] = true
	}
	return &writable{memoryStore: store}
}

func TestGivingWritesOneEntryAndOnlyForWhatTheTenantHas(t *testing.T) {
	ctx := context.Background()
	s := tenantStore(t, "demo")
	for name, c := range map[string]struct {
		a    Assignment
		want Tuple
	}{
		"a position":           {Assignment{Position: "user"}, Tuple{User: "user:anna", Relation: "holder", Object: "position:demo/user"}},
		"a role on the tenant": {Assignment{Role: "admin"}, Tuple{User: "user:anna", Relation: "admin", Object: "tenant:demo"}},
		"a role on an app":     {Assignment{App: "wiki", Role: "user"}, Tuple{User: "user:anna", Relation: "user", Object: "app:demo/wiki"}},
		"a role on an add-on":  {Assignment{App: "shop-sales", Role: "admin"}, Tuple{User: "user:anna", Relation: "admin", Object: "app:demo/shop-sales"}},
		"a declared role":      {Assignment{App: "shop-sales", Role: "manager"}, Tuple{User: "user:anna", Relation: "holder", Object: "role:demo/shop-sales/manager"}},
	} {
		changed, err := Give(ctx, s, "demo", "anna", c.a)
		if err != nil || !changed || !s.memoryStore[c.want] {
			t.Errorf("%s: changed %v, err %v, held %v", name, changed, err, s.memoryStore[c.want])
		}
		// Given again, nothing is written.
		before := s.writes
		if changed, err := Give(ctx, s, "demo", "anna", c.a); err != nil || changed || s.writes != before {
			t.Errorf("%s, given again: changed %v, err %v", name, changed, err)
		}
	}
	got, err := Assignments(ctx, s, "demo", "anna")
	if err != nil {
		t.Fatal(err)
	}
	want := []Assignment{
		{Position: "user"}, {Role: "admin"},
		{App: "shop-sales", Role: "admin"}, {App: "shop-sales", Role: "manager"}, {App: "wiki", Role: "user"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("assignments = %+v\nwant %+v", got, want)
	}

	// What the tenant does not have is not given, and nothing is written.
	before := s.writes
	for name, a := range map[string]Assignment{
		"a position nobody declared":      {Position: "ghost"},
		"an app that is not installed":    {App: "notes", Role: "user"},
		"a role the app does not declare": {App: "shop-sales", Role: "ghost"},
		"a role declared by another app":  {App: "wiki", Role: "manager"},
	} {
		if _, err := Give(ctx, s, "demo", "anna", a); !errors.Is(err, ErrNoSuchThing) {
			t.Errorf("%s: err = %v, want ErrNoSuchThing", name, err)
		}
	}
	// What is no assignment is refused before the store is asked.
	for name, a := range map[string]Assignment{
		"nothing":                         {},
		"a position and a role":           {Position: "user", Role: "admin"},
		"a position on an app":            {Position: "user", App: "wiki"},
		"an app and no role":              {App: "wiki"},
		"a tenant role that is none":      {Role: "manager"},
		"the claim's role on the tenant":  {Role: "auditor"},
		"a position that is no name":      {Position: "a/b"},
		"an app that reaches elsewhere":   {App: "../other", Role: "user"},
		"a role that carries a separator": {App: "wiki", Role: "user#x"},
	} {
		if _, err := Give(ctx, s, "demo", "anna", a); !errors.Is(err, ErrInvalidID) {
			t.Errorf("%s: err = %v, want ErrInvalidID", name, err)
		}
	}
	if _, err := Give(ctx, s, "de/mo", "anna", Assignment{Position: "user"}); !errors.Is(err, ErrInvalidID) {
		t.Errorf("a tenant that is no name: %v", err)
	}
	if _, err := Give(ctx, s, "demo", "an na", Assignment{Position: "user"}); !errors.Is(err, ErrInvalidID) {
		t.Errorf("a person that cannot be named: %v", err)
	}
	if s.writes != before {
		t.Errorf("%d entries written for what was refused", s.writes-before)
	}
	// A tenant the store does not know has nothing to give.
	if _, err := Give(ctx, s, "other", "anna", Assignment{Role: "user"}); !errors.Is(err, ErrNoSuchThing) {
		t.Errorf("a tenant that is not attached: %v", err)
	}
	// Another tenant's position of the same name is its own.
	if _, err := Give(ctx, s, "other", "anna", Assignment{Position: "user"}); !errors.Is(err, ErrNoSuchThing) {
		t.Errorf("another tenant's position: %v", err)
	}
}

func TestTakingRemovesWhatIsHeldAndNothingElse(t *testing.T) {
	ctx := context.Background()
	s := tenantStore(t, "demo")
	for _, a := range []Assignment{{Position: "user"}, {App: "wiki", Role: "user"}, {App: "shop-sales", Role: "manager"}} {
		if _, err := Give(ctx, s, "demo", "anna", a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Give(ctx, s, "demo", "ben", Assignment{Position: "user"}); err != nil {
		t.Fatal(err)
	}
	changed, err := Take(ctx, s, "demo", "anna", Assignment{App: "wiki", Role: "user"})
	if err != nil || !changed {
		t.Fatalf("taken: %v, %v", changed, err)
	}
	if changed, err := Take(ctx, s, "demo", "anna", Assignment{App: "wiki", Role: "user"}); err != nil || changed {
		t.Errorf("taken again: %v, %v", changed, err)
	}
	// What the position holds is the position's, and stays.
	if !s.memoryStore[Tuple{User: "position:demo/user#holder", Relation: "user", Object: "app:demo/wiki"}] {
		t.Error("taking a person's role removed the position's")
	}
	n, err := RemovePerson(ctx, s, "demo", "anna")
	if err != nil || n != 2 {
		t.Fatalf("a person removed: %d entries, %v", n, err)
	}
	if left, _ := Assignments(ctx, s, "demo", "anna"); len(left) != 0 {
		t.Errorf("left of a removed person: %+v", left)
	}
	if left, _ := Assignments(ctx, s, "demo", "ben"); len(left) != 1 {
		t.Errorf("another person's: %+v", left)
	}
	if n, err := RemovePerson(ctx, s, "demo", "anna"); err != nil || n != 0 {
		t.Errorf("removed again: %d, %v", n, err)
	}
}

// A tenant keeps an administrator of its own: the last one is neither taken
// the role nor removed, however they hold it.
func TestTheLastAdministratorKeepsTheRole(t *testing.T) {
	ctx := context.Background()
	give := func(s *writable, sub string, a Assignment) {
		t.Helper()
		if _, err := Give(ctx, s, "demo", sub, a); err != nil {
			t.Fatal(err)
		}
	}
	for name, a := range map[string]Assignment{
		"through the position every tenant has":     {Position: PositionTenantAdministrator},
		"through a position of the tenant's own":    {Position: "deputy"},
		"through a position that includes that one": {Position: "chief"},
		"given the role directly":                   {Role: RoleAdmin},
	} {
		s := tenantStore(t, "demo")
		give(s, "tom", a)
		give(s, "tom", Assignment{Position: PositionUser})
		if got, _ := Administrators(ctx, s, "demo"); strings.Join(got, ",") != "tom" {
			t.Errorf("%s: administrators = %v", name, got)
		}
		if _, err := Take(ctx, s, "demo", "tom", a); !errors.Is(err, ErrLastAdministrator) {
			t.Errorf("%s: taken from the last administrator: %v", name, err)
		}
		if n, err := RemovePerson(ctx, s, "demo", "tom"); !errors.Is(err, ErrLastAdministrator) || n != 0 {
			t.Errorf("%s: the last administrator removed: %d, %v", name, n, err)
		}
		if left, _ := Assignments(ctx, s, "demo", "tom"); len(left) != 2 {
			t.Errorf("%s: something of the last administrator's was taken: %+v", name, left)
		}
		// What is not administering is theirs to lose.
		if changed, err := Take(ctx, s, "demo", "tom", Assignment{Position: PositionUser}); err != nil || !changed {
			t.Errorf("%s: another position of the last administrator: %v, %v", name, changed, err)
		}
		// With a second administrator, the first may go.
		give(s, "pat", Assignment{Position: PositionTenantAdministrator})
		if n, err := RemovePerson(ctx, s, "demo", "tom"); err != nil || n != 1 {
			t.Errorf("%s: with another administrator: %d, %v", name, n, err)
		}
		if got, _ := Administrators(ctx, s, "demo"); strings.Join(got, ",") != "pat" {
			t.Errorf("%s: administrators after = %v", name, got)
		}
	}

	// Holding it twice, one may be taken.
	s := tenantStore(t, "demo")
	give(s, "tom", Assignment{Position: PositionTenantAdministrator})
	give(s, "tom", Assignment{Role: RoleAdmin})
	if changed, err := Take(ctx, s, "demo", "tom", Assignment{Role: RoleAdmin}); err != nil || !changed {
		t.Errorf("one of two ways taken: %v, %v", changed, err)
	}
	// An app's administrator is not the tenant's.
	give(s, "odin", Assignment{Position: PositionAppAdministrator})
	if got, _ := Administrators(ctx, s, "demo"); strings.Join(got, ",") != "tom" {
		t.Errorf("administrators = %v", got)
	}
	// A tenant nobody administers is not made worse by taking.
	empty := tenantStore(t, "demo")
	give(empty, "anna", Assignment{Position: PositionUser})
	if changed, err := Take(ctx, empty, "demo", "anna", Assignment{Position: PositionUser}); err != nil || !changed {
		t.Errorf("in a tenant with no administrator: %v, %v", changed, err)
	}
}

// Against the model: every kind of assignment is one the store takes, and
// what is given answers.
func TestWhatIsGivenAnswersInTheStore(t *testing.T) {
	o := requireOpenFGA(t)
	ctx := context.Background()
	store, model, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	o.StoreID, o.ModelID = store, model
	c, err := NewOpenFGA(o)
	if err != nil {
		t.Fatal(err)
	}
	const tenant = "give-a"
	apps := []InstalledApp{{Profile: "wiki"}, {Profile: "shop", Addons: []InstalledApp{
		{Profile: "shop-sales", Roles: []DeclaredRole{{Name: "manager"}}},
	}}}
	if err := c.ReconcileTenants(ctx, "give-cluster", TenantsNamed(tenant)); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileApps(ctx, map[string][]InstalledApp{tenant: apps}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcilePositions(ctx, map[string][]PositionSpec{tenant: ShippedPositions(apps)}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []Assignment{
		{Position: PositionTenantAdministrator}, {Position: PositionUser}, {Role: RoleUser}, {Role: RoleAdmin},
		{App: "wiki", Role: RoleUser}, {App: "shop", Role: RoleAdmin}, {App: "shop-sales", Role: "manager"},
	} {
		if changed, err := Give(ctx, c, tenant, "g-anna", a); err != nil || !changed {
			t.Fatalf("give %+v: %v, %v", a, changed, err)
		}
	}
	held, err := Assignments(ctx, c, tenant, "g-anna")
	if err != nil || len(held) != 7 {
		t.Fatalf("assignments = %+v, %v", held, err)
	}
	for _, q := range [][2]string{
		{"can_manage_users", Tenant(tenant)}, {"can_use", App(tenant, "wiki")},
		{"can_administer", App(tenant, "shop-sales")}, {"holder", Role(tenant, "shop-sales", "manager")},
	} {
		if ok, err := c.Check(ctx, "test", "user:g-anna", q[0], q[1]); err != nil || !ok {
			t.Errorf("%s on %s = %v, %v", q[0], q[1], ok, err)
		}
	}
	if got, err := Administrators(ctx, c, tenant); err != nil || strings.Join(got, ",") != "g-anna" {
		t.Errorf("administrators = %v, %v", got, err)
	}
	if n, err := RemovePerson(ctx, c, tenant, "g-anna"); !errors.Is(err, ErrLastAdministrator) || n != 0 {
		t.Errorf("the last administrator removed: %d, %v", n, err)
	}
	if _, err := Give(ctx, c, tenant, "g-ben", Assignment{Position: PositionTenantAdministrator}); err != nil {
		t.Fatal(err)
	}
	if n, err := RemovePerson(ctx, c, tenant, "g-anna"); err != nil || n != 7 {
		t.Errorf("removed: %d, %v", n, err)
	}
	if ok, _ := c.Check(ctx, "test", "user:g-anna", "can_use", App(tenant, "wiki")); ok {
		t.Error("a removed person still uses an app")
	}
}

// An app that is told its people is told who administers it and who may use
// it: given directly or through a position, with the base's administrators
// for an add-on and the holders of its declared roles among its users.
func TestAnAppsHoldersAreReadThroughPositions(t *testing.T) {
	ctx := context.Background()
	s := tenantStore(t, "demo")
	give := func(sub string, a Assignment) {
		t.Helper()
		if _, err := Give(ctx, s, "demo", sub, a); err != nil {
			t.Fatal(err)
		}
	}
	give("anna", Assignment{Position: PositionAppAdministrator})
	give("ben", Assignment{App: "shop-sales", Role: "manager"})
	give("cleo", Assignment{App: "shop-sales", Role: RoleUser})
	give("dan", Assignment{App: "shop", Role: RoleAdmin})
	give("eve", Assignment{Position: PositionUser})

	admins, users, err := AppHolders(ctx, s, "demo", "shop-sales")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(admins, ",") != "anna,dan" {
		t.Errorf("administrators of the add-on = %v, want the base's: anna through her position, dan directly", admins)
	}
	if strings.Join(users, ",") != "anna,ben,cleo,dan" {
		t.Errorf("users of the add-on = %v", users)
	}
	_, users, err = AppHolders(ctx, s, "demo", "wiki")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(users, ",") != "anna,eve" {
		t.Errorf("users of an app for everyone = %v, want its administrator and the holder of User", users)
	}
	if _, _, err := AppHolders(ctx, s, "demo", "no/such"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("err = %v, want ErrInvalidID", err)
	}
}

// The people of a tenant are the holders of its position user, and a
// position's holders include those of a position that includes it.
func TestAPositionsHoldersIncludeThoseOfAPositionThatIncludesIt(t *testing.T) {
	ctx := context.Background()
	s := tenantStore(t, "demo")
	for sub, position := range map[string]string{"anna": PositionUser, "ben": "chief", "cleo": "deputy"} {
		if _, err := Give(ctx, s, "demo", sub, Assignment{Position: position}); err != nil {
			t.Fatal(err)
		}
	}
	people, err := PositionHolders(ctx, s, "demo", PositionUser)
	if err != nil || strings.Join(people, ",") != "anna" {
		t.Errorf("people = %v, err = %v", people, err)
	}
	deputies, err := PositionHolders(ctx, s, "demo", "deputy")
	if err != nil || strings.Join(deputies, ",") != "ben,cleo" {
		t.Errorf("deputies = %v, err = %v, want the chief too: chief includes deputy", deputies, err)
	}
}

// Whether a person holds one of some positions counts a position given to
// them and one a given position includes, and no other tenant's.
func TestHoldsAnyPositionCountsInclusion(t *testing.T) {
	ctx := context.Background()
	s := tenantStore(t, "acme")
	if _, err := Give(ctx, s, "acme", "anna", Assignment{Position: "chief"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tenant, sub string
		positions   []string
		want        bool
	}{
		{"acme", "anna", []string{"chief"}, true},
		{"acme", "anna", []string{"deputy"}, true},
		{"acme", "anna", []string{"user", "deputy"}, true},
		{"acme", "anna", []string{"user"}, false},
		{"acme", "anna", nil, false},
		{"acme", "ben", []string{"deputy"}, false},
		{"other", "anna", []string{"chief"}, false},
	} {
		got, err := HoldsAnyPosition(ctx, s, c.tenant, c.sub, c.positions)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s holds one of %v in %s: %v, want %v", c.sub, c.positions, c.tenant, got, c.want)
		}
	}
	// Who holds deputy only does not hold chief: inclusion goes one way.
	if _, err := Give(ctx, s, "acme", "carl", Assignment{Position: "deputy"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := HoldsAnyPosition(ctx, s, "acme", "carl", []string{"chief"}); got {
		t.Error("a holder of an included position holds the one that includes it")
	}
}
