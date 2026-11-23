/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// positionsOf are the entries that make the positions exist in a tenant: a
// position is given only when the tenant has it.
func declaredPositions(tenant string, names ...string) []authz.Tuple {
	out := make([]authz.Tuple, 0, len(names))
	for _, n := range names {
		out = append(out, authz.Tuple{User: authz.Tenant(tenant), Relation: "tenant", Object: authz.Position(tenant, n)})
	}
	return out
}

func userOf(t *testing.T, sub string) string {
	t.Helper()
	u, err := authz.User(sub)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func arrivalsFor(t *testing.T, store *fakeRights, tenants ...*gentianov1alpha1.Tenant) *RealmArrivals {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(appStoreScheme(t))
	for _, tenant := range tenants {
		b = b.WithObjects(tenant)
	}
	return &RealmArrivals{Client: b.Build(), Rights: store}
}

// An account a person made themself is given what spec.signUp.positions
// names, never an administrator's, and only where sign-up is open.
func TestSignUpPositionsAreGivenToWhoRegisters(t *testing.T) {
	ctx := context.Background()
	shop := &gentianov1alpha1.Tenant{}
	shop.Name = "shop"
	shop.Spec.SignUp = &gentianov1alpha1.TenantSignUp{Enabled: true,
		Positions: []string{"customer", "tenant-administrator", "gone"}}
	closed := &gentianov1alpha1.Tenant{}
	closed.Name = "closed"
	closed.Spec.SignUp = &gentianov1alpha1.TenantSignUp{Positions: []string{"customer"}}

	tuples := append(declaredPositions("shop", "customer", "tenant-administrator"), declaredPositions("closed", "customer")...)
	store := newFakeRights(tuples...)
	a := arrivalsFor(t, store, shop, closed)

	if err := a.PersonRegistered(ctx, "shop", "u-1"); err != nil {
		t.Fatal(err)
	}
	u := userOf(t, "u-1")
	if !store.has(u, "holder", authz.Position("shop", "customer")) {
		t.Error("a person who registered is not given the sign-up position")
	}
	if store.has(u, "holder", authz.Position("shop", "tenant-administrator")) {
		t.Error("a person who registered is given an administrator's position")
	}
	if err := a.PersonRegistered(ctx, "closed", "u-2"); err != nil {
		t.Fatal(err)
	}
	if store.has(userOf(t, "u-2"), "holder", authz.Position("closed", "customer")) {
		t.Error("a tenant whose sign-up is closed gives its sign-up positions")
	}
}

// An entry of spec.signInFrom with holding gives its positions to a person
// who holds one of the named positions in the source tenant, directly or
// through an inclusion; takes them from one who does not; and does neither
// when the realm did not say who the person is at the source. An entry
// without holding gives its positions to everybody and never takes them.
func TestSignInFromHoldingGivesAndTakesByWhatIsHeldAtHome(t *testing.T) {
	ctx := context.Background()
	shop := &gentianov1alpha1.Tenant{}
	shop.Name = "shop"
	shop.Spec.SignInFrom = []gentianov1alpha1.TenantSignInFrom{
		{Tenant: "team", Positions: []string{"editor", "app-administrator"}, Holding: []string{"sales"}},
		{Tenant: "partners", Positions: []string{"user"}},
	}

	tuples := declaredPositions("shop", "editor", "user", "app-administrator")
	tuples = append(tuples, declaredPositions("team", "sales", "executive")...)
	// executive includes sales.
	tuples = append(tuples, authz.Tuple{User: authz.Position("team", "executive"), Relation: "included_in",
		Object: authz.Position("team", "sales")})
	store := newFakeRights(tuples...)
	a := arrivalsFor(t, store, shop)
	editor := authz.Position("shop", "editor")

	// At home: anna holds sales, ben is an executive, carl holds nothing.
	for sub, position := range map[string]string{"team-anna": "sales", "team-ben": "executive"} {
		store.tuples[authz.Tuple{User: userOf(t, sub), Relation: "holder", Object: authz.Position("team", position)}] = true
	}

	for _, c := range []struct{ sub, home string }{{"anna", "team-anna"}, {"ben", "team-ben"}} {
		if err := a.PersonEntered(ctx, "shop", c.sub, "tenant-team", c.home); err != nil {
			t.Fatal(err)
		}
		if !store.has(userOf(t, c.sub), "holder", editor) {
			t.Errorf("%s holds sales at home and is not given editor", c.sub)
		}
		if store.has(userOf(t, c.sub), "holder", authz.Position("shop", "app-administrator")) {
			t.Errorf("%s is given an administrator's position", c.sub)
		}
	}

	// carl holds nothing at home, but was given editor before.
	carl := userOf(t, "carl")
	store.tuples[authz.Tuple{User: carl, Relation: "holder", Object: editor}] = true
	if err := a.PersonEntered(ctx, "shop", "carl", "tenant-team", "team-carl"); err != nil {
		t.Fatal(err)
	}
	if store.has(carl, "holder", editor) {
		t.Error("a person who holds nothing at home keeps editor")
	}

	// anna leaves sales: at her next arrival, editor goes.
	delete(store.tuples, authz.Tuple{User: userOf(t, "team-anna"), Relation: "holder", Object: authz.Position("team", "sales")})
	if err := a.PersonEntered(ctx, "shop", "anna", "tenant-team", "team-anna"); err != nil {
		t.Fatal(err)
	}
	if store.has(userOf(t, "anna"), "holder", editor) {
		t.Error("a person who left sales keeps editor")
	}

	// Nobody said who ben is at home: nothing given, nothing taken.
	if err := a.PersonEntered(ctx, "shop", "ben", "tenant-team", ""); err != nil {
		t.Fatal(err)
	}
	if !store.has(userOf(t, "ben"), "holder", editor) {
		t.Error("an arrival that does not say who the person is at home took editor")
	}
	if err := a.PersonEntered(ctx, "shop", "dora", "tenant-team", ""); err != nil {
		t.Fatal(err)
	}
	if store.has(userOf(t, "dora"), "holder", editor) {
		t.Error("an arrival that does not say who the person is at home gave editor")
	}

	// The entry without holding: everybody is given user, and an arrival
	// takes nothing from them.
	if err := a.PersonEntered(ctx, "shop", "eve", "tenant-partners", "partners-eve"); err != nil {
		t.Fatal(err)
	}
	eve := userOf(t, "eve")
	if !store.has(eve, "holder", authz.Position("shop", "user")) {
		t.Error("an entry without holding does not give its positions")
	}
	store.tuples[authz.Tuple{User: eve, Relation: "holder", Object: editor}] = true
	if err := a.PersonEntered(ctx, "shop", "eve", "tenant-partners", "partners-eve"); err != nil {
		t.Fatal(err)
	}
	if !store.has(eve, "holder", editor) || !store.has(eve, "holder", authz.Position("shop", "user")) {
		t.Error("an arrival through an entry without holding took a position")
	}
}
