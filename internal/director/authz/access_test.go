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
	"strings"
	"testing"
)

// store answers reads from a list of tuples as OpenFGA's read does: a filter
// field that is set must match, and an object that ends in ':' names a type.
func store(tuples ...Tuple) readFunc {
	return func(_ context.Context, f Tuple) ([]Tuple, error) {
		var out []Tuple
		for _, t := range tuples {
			if f.User != "" && f.User != t.User {
				continue
			}
			if f.Relation != "" && f.Relation != t.Relation {
				continue
			}
			if strings.HasSuffix(f.Object, ":") {
				if !strings.HasPrefix(t.Object, f.Object) {
					continue
				}
			} else if f.Object != "" && f.Object != t.Object {
				continue
			}
			out = append(out, t)
		}
		return out, nil
	}
}

// answering says yes to the questions listed as "user relation object", and
// records every question asked.
func answering(asked *[]Question, yes ...string) askFunc {
	return func(_ context.Context, questions []Question) ([]bool, error) {
		out := make([]bool, len(questions))
		for i, q := range questions {
			*asked = append(*asked, q)
			for _, y := range yes {
				if y == q.User+" "+q.Relation+" "+q.Object {
					out[i] = true
				}
			}
		}
		return out, nil
	}
}

func permission(t *testing.T, access ObjectAccess, name string) Permission {
	t.Helper()
	for _, p := range access.Permissions {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no permission %s on %s: %+v", name, access.Object, access.Permissions)
	return Permission{}
}

var demo = []Tuple{
	// who holds what
	{User: "user:anna", Relation: "holder", Object: "position:demo/user"},
	{User: "user:anna", Relation: "user", Object: "app:demo/notes"},
	{User: "user:ben", Relation: "holder", Object: "position:demo/tenant-administrator"},
	{User: "user:eve", Relation: "holder", Object: "position:demo/executive"},
	{User: "user:marc", Relation: "holder", Object: "role:demo/shop-sales/configurator"},
	{User: "user:odin", Relation: "holder", Object: "position:demo/app-administrator"},
	{User: "user:olga", Relation: "holder", Object: "position:other/user"},
	// the tenant and its positions
	{User: "cluster:c1", Relation: "cluster", Object: "tenant:demo"},
	{User: "tenant:demo", Relation: "tenant", Object: "position:demo/user"},
	{User: "tenant:demo", Relation: "tenant", Object: "position:demo/tenant-administrator"},
	{User: "tenant:demo", Relation: "tenant", Object: "position:demo/app-administrator"},
	{User: "tenant:demo", Relation: "tenant", Object: "position:demo/sales-manager"},
	{User: "tenant:demo", Relation: "tenant", Object: "position:demo/executive"},
	{User: "position:demo/executive", Relation: "included_in", Object: "position:demo/sales-manager"},
	{User: "position:demo/user#holder", Relation: "user", Object: "tenant:demo"},
	{User: "position:demo/tenant-administrator#holder", Relation: "admin", Object: "tenant:demo"},
	{User: "position:demo/sales-manager#holder", Relation: "user", Object: "tenant:demo"},
	{User: "position:demo/sales-manager#holder", Relation: "holder", Object: "role:demo/shop-sales/manager"},
	{User: "position:demo/app-administrator#holder", Relation: "admin", Object: "app:demo/shop"},
	{User: "position:other/user#holder", Relation: "user", Object: "tenant:other"},
	{User: "user:demo/cara", Relation: "auditor", Object: "tenant:demo"},
	// its apps: notes, and shop with its add-on shop-sales and the roles it declares
	{User: "tenant:demo", Relation: "tenant", Object: "app:demo/notes"},
	{User: "tenant:demo", Relation: "tenant", Object: "app:demo/shop"},
	{User: "tenant:demo", Relation: "tenant", Object: "app:demo/shop-sales"},
	{User: "app:demo/shop", Relation: "base", Object: "app:demo/shop-sales"},
	{User: "app:demo/shop-sales", Relation: "addon", Object: "app:demo/shop"},
	{User: "app:demo/shop-sales", Relation: "app", Object: "role:demo/shop-sales/manager"},
	{User: "app:demo/shop-sales", Relation: "app", Object: "role:demo/shop-sales/configurator"},
	{User: "role:demo/shop-sales/manager", Relation: "declared", Object: "app:demo/shop-sales"},
	{User: "role:demo/shop-sales/configurator", Relation: "declared", Object: "app:demo/shop-sales"},
	{User: "tenant:other", Relation: "tenant", Object: "app:other/notes"},
	{User: "user:platform/dora", Relation: "auditor", Object: "cluster:c1"},
}

func appOf(t *testing.T, access PersonAccess, profile string) ObjectAccess {
	t.Helper()
	for _, a := range access.Apps {
		if a.App == profile {
			return a
		}
	}
	t.Fatalf("no app %s in %+v", profile, access.Apps)
	return ObjectAccess{}
}

// A person's rights are traced to the position they are held through, a role
// given directly is told apart, and every app of the tenant is listed, also
// the ones they hold nothing on.
func TestAPersonsAccessNamesThePositionEachRoleIsHeldThrough(t *testing.T) {
	var asked []Question
	got, err := personAccess(context.Background(), store(demo...), answering(&asked,
		"user:anna user tenant:demo", "user:anna can_enter tenant:demo", "user:anna can_view tenant:demo",
		"user:anna user app:demo/notes", "user:anna can_use app:demo/notes", "user:anna can_launch app:demo/notes",
	), "c1", "demo", "anna")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Positions) != 1 || got.Positions[0] != (PositionHeld{Name: "user"}) {
		t.Fatalf("positions = %+v", got.Positions)
	}
	if len(got.OnTenant.Held) != 1 || got.OnTenant.Held[0].Role != "user" || got.OnTenant.Held[0].Position != "user" || got.OnTenant.Held[0].Direct {
		t.Fatalf("held on the tenant = %+v", got.OnTenant.Held)
	}
	if strings.Join(got.OnTenant.Roles, ",") != "user" {
		t.Errorf("roles in effect on the tenant = %v", got.OnTenant.Roles)
	}
	if len(got.Apps) != 3 {
		t.Fatalf("apps = %+v, want all three of the tenant and none of another's", got.Apps)
	}
	notes := appOf(t, got, "notes")
	if len(notes.Held) != 1 || notes.Held[0].Role != "user" || !notes.Held[0].Direct || notes.Held[0].Position != "" {
		t.Errorf("held on notes = %+v, want user, given directly", notes.Held)
	}
	if p := permission(t, notes, "can_use"); !p.InEffect || p.From != FromRole {
		t.Errorf("can_use on notes = %+v", p)
	}
	shop := appOf(t, got, "shop")
	if len(shop.Held) != 0 || len(shop.Roles) != 0 {
		t.Errorf("held on shop = %+v, roles %v, want nothing", shop.Held, shop.Roles)
	}
	if p := permission(t, shop, "can_use"); p.InEffect || p.From != "" {
		t.Errorf("can_use on shop = %+v, want not in effect and from nothing", p)
	}
	for _, q := range asked {
		if !strings.HasSuffix(q.Object, "demo") && !strings.Contains(q.Object, ":demo/") {
			t.Errorf("asked about %s, which is not the tenant's", q.Object)
		}
	}
}

// Whoever holds Executive holds what Sales manager holds, and is told so:
// the position they were given, and the one it includes.
func TestAPositionThatIncludesAnotherBringsItsRoles(t *testing.T) {
	var asked []Question
	got, err := personAccess(context.Background(), store(demo...), answering(&asked,
		"user:eve user tenant:demo", "user:eve can_enter tenant:demo",
		"user:eve holder role:demo/shop-sales/manager", "user:eve user app:demo/shop-sales", "user:eve can_use app:demo/shop-sales",
		"user:eve user app:demo/shop", "user:eve can_use app:demo/shop",
	), "c1", "demo", "eve")
	if err != nil {
		t.Fatal(err)
	}
	want := []PositionHeld{{Name: "executive"}, {Name: "sales-manager", Through: "executive"}}
	if len(got.Positions) != 2 || got.Positions[0] != want[0] || got.Positions[1] != want[1] {
		t.Fatalf("positions = %+v, want %+v", got.Positions, want)
	}
	sales := appOf(t, got, "shop-sales")
	if len(sales.Held) != 1 || sales.Held[0].Role != "manager" || !sales.Held[0].Declared || sales.Held[0].Position != "sales-manager" {
		t.Fatalf("held on the add-on = %+v, want the declared role manager through sales-manager", sales.Held)
	}
	if strings.Join(sales.Roles, ",") != "manager,user" {
		t.Errorf("roles in effect on the add-on = %v", sales.Roles)
	}
	if p := permission(t, sales, "can_use"); !p.InEffect || p.From != FromRole {
		t.Errorf("can_use on the add-on = %+v, want carried by the declared role", p)
	}
	// On the base nothing is held; its use follows from the add-on.
	shop := appOf(t, got, "shop")
	if len(shop.Held) != 0 {
		t.Errorf("held on the base = %+v, want nothing", shop.Held)
	}
	if p := permission(t, shop, "can_use"); !p.InEffect || p.From != FromApp {
		t.Errorf("can_use on the base = %+v, want from the add-on", p)
	}
}

// A role given beside a position is shown as given directly, on the app
// that declares it.
func TestARoleGivenDirectlyIsToldApart(t *testing.T) {
	var asked []Question
	got, err := personAccess(context.Background(), store(demo...), answering(&asked,
		"user:marc holder role:demo/shop-sales/configurator", "user:marc user app:demo/shop-sales",
	), "", "demo", "marc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Positions) != 0 {
		t.Errorf("positions = %+v, want none", got.Positions)
	}
	sales := appOf(t, got, "shop-sales")
	if len(sales.Held) != 1 || sales.Held[0].Role != "configurator" || !sales.Held[0].Direct || !sales.Held[0].Declared {
		t.Fatalf("held = %+v", sales.Held)
	}
	if strings.Join(sales.Roles, ",") != "configurator,user" {
		t.Errorf("roles in effect = %v", sales.Roles)
	}
}

// The admin of a base app administers its add-on: nothing is held there,
// and the page says the permission follows from the other app.
func TestWhatFollowsFromTheBaseOrTheTenantIsSaid(t *testing.T) {
	var asked []Question
	got, err := personAccess(context.Background(), store(demo...), answering(&asked,
		"user:odin admin app:demo/shop", "user:odin user app:demo/shop", "user:odin can_administer app:demo/shop", "user:odin can_use app:demo/shop",
		"user:odin admin app:demo/shop-sales", "user:odin user app:demo/shop-sales", "user:odin can_administer app:demo/shop-sales",
		"user:odin holder role:demo/shop-sales/manager",
	), "", "demo", "odin")
	if err != nil {
		t.Fatal(err)
	}
	shop := appOf(t, got, "shop")
	if len(shop.Held) != 1 || shop.Held[0].Role != "admin" || shop.Held[0].Position != "app-administrator" {
		t.Fatalf("held on the base = %+v", shop.Held)
	}
	sales := appOf(t, got, "shop-sales")
	if len(sales.Held) != 0 {
		t.Errorf("held on the add-on = %+v, want nothing", sales.Held)
	}
	if p := permission(t, sales, "can_administer"); !p.InEffect || p.From != FromApp {
		t.Errorf("can_administer on the add-on = %+v, want from the base", p)
	}
	if strings.Join(sales.Roles, ",") != "admin,manager,user" {
		t.Errorf("roles in effect on the add-on = %v", sales.Roles)
	}

	// A tenant's admin writes an app's credentials, and holds nothing on it.
	asked = nil
	got, err = personAccess(context.Background(), store(demo...), answering(&asked,
		"user:ben admin tenant:demo", "user:ben user tenant:demo", "user:ben can_manage_users tenant:demo",
		"user:ben can_write_credential app:demo/notes",
	), "", "demo", "ben")
	if err != nil {
		t.Fatal(err)
	}
	notes := appOf(t, got, "notes")
	if p := permission(t, notes, "can_write_credential"); !p.InEffect || p.From != FromTenant {
		t.Errorf("can_write_credential on notes = %+v, want from the tenant", p)
	}
	if p := permission(t, notes, "can_use"); p.InEffect {
		t.Errorf("can_use on notes = %+v: a tenant's admin holds no role on its apps", p)
	}
}

// A role the claim gives by name is stored for the account of its realm, and
// is found and asked about as that account.
func TestARoleHeldByNameIsFound(t *testing.T) {
	var asked []Question
	got, err := personAccess(context.Background(), store(demo...), answering(&asked,
		"user:demo/cara auditor tenant:demo", "user:demo/cara can_read_evidence tenant:demo",
	), "c1", "demo", "cara")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.OnTenant.Held) != 1 || got.OnTenant.Held[0].Role != "auditor" || !got.OnTenant.Held[0].Direct {
		t.Fatalf("held = %+v", got.OnTenant.Held)
	}
	if p := permission(t, got.OnTenant, "can_read_evidence"); !p.InEffect || p.From != FromRole {
		t.Errorf("can_read_evidence = %+v", p)
	}

	// On the cluster only: nothing is held in the tenant, and what is in
	// effect there is said to come from elsewhere.
	asked = nil
	got, err = personAccess(context.Background(), store(demo...), answering(&asked,
		"user:platform/dora can_view tenant:demo",
	), "c1", "demo", "dora")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.OnTenant.Held) != 0 {
		t.Errorf("held = %+v, want nothing: the role is the cluster's", got.OnTenant.Held)
	}
	if p := permission(t, got.OnTenant, "can_view"); !p.InEffect || p.From != FromElsewhere {
		t.Errorf("can_view = %+v, want in effect from elsewhere", p)
	}
}

func TestAPersonWhoCannotBeNamedIsRefused(t *testing.T) {
	var asked []Question
	if _, err := personAccess(context.Background(), store(demo...), answering(&asked), "", "demo", "a b"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("err = %v, want ErrInvalidID", err)
	}
}

// A position is what it holds, what it includes, and who was given it.
func TestAPositionIsWhatItHoldsAndWhoHoldsIt(t *testing.T) {
	got, err := positionAccess(context.Background(), store(demo...), "demo", "sales-manager")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Holds) != 2 {
		t.Fatalf("holds = %+v", got.Holds)
	}
	if h := got.Holds[0]; h.Object != "app:demo/shop-sales" || h.App != "shop-sales" || h.Role != "manager" || !h.Declared {
		t.Errorf("first hold = %+v, want the declared role on the add-on", h)
	}
	if h := got.Holds[1]; h.Object != "tenant:demo" || h.Role != "user" || !contains(h.Carries, "can_enter") {
		t.Errorf("second hold = %+v, want user on the tenant", h)
	}
	if len(got.Inherited) != 0 {
		t.Errorf("sales-manager inherits %+v, includes nothing", got.Inherited)
	}
	if strings.Join(got.IncludedIn, ",") != "executive" || len(got.Includes) != 0 {
		t.Errorf("includes %v, included in %v", got.Includes, got.IncludedIn)
	}

	got, err = positionAccess(context.Background(), store(demo...), "demo", "executive")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Includes, ",") != "sales-manager" || strings.Join(got.Holders, ",") != "eve" {
		t.Errorf("executive includes %v, held by %v", got.Includes, got.Holders)
	}
	// Executive holds nothing itself, and what Sales manager holds through
	// the inclusion, each role naming the position it comes from.
	if len(got.Holds) != 0 || len(got.Inherited) != 2 {
		t.Fatalf("executive holds %+v, inherits %+v", got.Holds, got.Inherited)
	}
	for _, h := range got.Inherited {
		if strings.Join(h.Through, ",") != "sales-manager" {
			t.Errorf("inherited %+v, want it through sales-manager", h)
		}
	}
	if h := got.Inherited[0]; h.App != "shop-sales" || h.Role != "manager" || !h.Declared {
		t.Errorf("first inherited = %+v, want the declared role on the add-on", h)
	}

	// Another tenant's position of the same name is not read.
	got, err = positionAccess(context.Background(), store(demo...), "other", "sales-manager")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Holds) != 0 || len(got.Holders) != 0 {
		t.Errorf("other tenant's position = %+v, want nothing", got)
	}
	if _, err := positionAccess(context.Background(), store(demo...), "demo", "Sales/Manager"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("err = %v, want ErrInvalidID for a name that is not one", err)
	}
}

// What an app is told a person is: the roles the store answers for them, the
// declared ones among them.
func TestAppRolesAreWhatTheStoreAnswers(t *testing.T) {
	var asked []Question
	got, err := appRoles(context.Background(), store(demo...), answering(&asked,
		"user:eve user app:demo/shop-sales", "user:eve holder role:demo/shop-sales/manager",
	), "demo", "shop-sales", "eve")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "manager,user" {
		t.Errorf("roles = %v", got)
	}
	if len(asked) != 4 {
		t.Errorf("asked %d questions, want admin, user and the two declared roles: %+v", len(asked), asked)
	}
	asked = nil
	got, err = appRoles(context.Background(), store(demo...), answering(&asked), "demo", "shop-sales", "anna")
	if err != nil || len(got) != 0 || got == nil {
		t.Errorf("roles = %v, err = %v, want an empty list", got, err)
	}
	if _, err := appRoles(context.Background(), store(demo...), answering(&asked), "demo", "no/such", "anna"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("err = %v, want ErrInvalidID", err)
	}
}

// A sign-in to an app carries the person's roles on the app and on each
// add-on activated in it, and leaves out what they hold nothing on.
func TestASignInCarriesTheRolesOnTheAppAndItsAddons(t *testing.T) {
	tuples := []Tuple{
		{User: "tenant:demo", Relation: "tenant", Object: "app:demo/shop"},
		{User: "tenant:demo", Relation: "tenant", Object: "app:demo/shop-sales"},
		{User: "tenant:demo", Relation: "tenant", Object: "app:demo/shop-stock"},
		{User: "app:demo/shop", Relation: "base", Object: "app:demo/shop-sales"},
		{User: "app:demo/shop", Relation: "base", Object: "app:demo/shop-stock"},
		{User: "app:demo/shop-sales", Relation: "app", Object: "role:demo/shop-sales/manager"},
		{User: "app:other/shop", Relation: "base", Object: "app:other/shop-sales"},
	}
	var asked []Question
	got, err := signInRoles(context.Background(), store(tuples...), answering(&asked,
		"user:eve user app:demo/shop",
		"user:eve user app:demo/shop-sales", "user:eve holder role:demo/shop-sales/manager",
	), "demo", "shop", "eve")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].App != "shop" || strings.Join(got[0].Roles, ",") != "user" ||
		got[1].App != "shop-sales" || strings.Join(got[1].Roles, ",") != "manager,user" {
		t.Errorf("roles = %+v, want shop: user and shop-sales: manager, user", got)
	}
	got, err = signInRoles(context.Background(), store(tuples...), answering(&asked), "demo", "shop", "anna")
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("roles = %+v, err = %v, want an empty list for a person who holds nothing", got, err)
	}
}

// An app's view lists who was given each role, the declared ones too.
func TestAnAppsViewListsItsDeclaredRoles(t *testing.T) {
	view, err := appView(context.Background(), store(demo...), "demo", "shop-sales")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Binding{}
	for _, b := range view.Bindings {
		by[b.Relation] = b
	}
	manager, ok := by["manager"]
	if !ok || !manager.Declared || len(manager.Holders) != 1 || manager.Holders[0] != (Holder{Kind: HolderPosition, Name: "sales-manager"}) {
		t.Fatalf("manager = %+v", manager)
	}
	configurator := by["configurator"]
	if len(configurator.Holders) != 1 || configurator.Holders[0] != (Holder{Kind: HolderPerson, Name: "marc"}) {
		t.Errorf("configurator = %+v", configurator)
	}
	if _, ok := by["admin"]; !ok {
		t.Errorf("bindings = %+v, want the model's roles too", view.Bindings)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
