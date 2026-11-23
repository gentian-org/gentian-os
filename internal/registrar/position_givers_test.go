/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/registrar"
)

const (
	checkoutOfShop = "key-of-the-shops-checkout"
	catalogOfShop  = "key-of-the-shops-catalog"
	givePosition   = "/v1/tenants/demo/actions/give-position"
	takePosition   = "/v1/tenants/demo/actions/take-position"
)

// The operator's list: demo lets the shop's checkout give sales-manager and
// user; solo lets it give user. The catalog of the same tenant is listed
// for nothing of demo's.
func giversList() []registrar.PositionGiver {
	return []registrar.PositionGiver{
		{
			Key: componentkeys.Key{Tenant: "shop", Component: "checkout", KeyHash: componentkeys.Hash(checkoutOfShop)},
			Gives: []componentkeys.PositionGrant{
				{Tenant: "demo", Positions: []string{"sales-manager", "user"}},
				{Tenant: "solo", Positions: []string{"user"}},
			},
		},
		{
			Key:   componentkeys.Key{Tenant: "shop", Component: "catalog", KeyHash: componentkeys.Hash(catalogOfShop)},
			Gives: []componentkeys.PositionGrant{{Tenant: "solo", Positions: []string{"sales-manager"}}},
		},
	}
}

// A component a tenant names gives and takes exactly the positions named
// for it there, to and from one of the tenant's people, through the
// operator, which records the component as who acted.
func TestAPositionGiverGivesAndTakesWhatTheTenantNames(t *testing.T) {
	f := newFakeIdentity("demo", "solo")
	h := startWithIdentity(t, f)
	h.givers.Set(giversList())

	status, body := h.do(t, http.MethodPost, givePosition, checkoutOfShop, `{"person":"u1","position":"sales-manager"}`)
	if status != http.StatusOK || body["changed"] != true || body["position"] != "sales-manager" {
		t.Fatalf("given: %d %v", status, body)
	}
	if got := h.rights.positionsOf("demo", "u1"); strings.Join(got, ",") != "sales-manager" {
		t.Fatalf("u1 holds %v in demo", got)
	}
	if last := h.rights.asked[len(h.rights.asked)-1]; !strings.HasSuffix(last, " as component:shop/checkout") {
		t.Errorf("the operator was asked %q", last)
	}
	status, body = h.do(t, http.MethodPost, takePosition, checkoutOfShop, `{"person":"u1","position":"sales-manager"}`)
	if status != http.StatusOK || body["changed"] != true || body["holds"] != false {
		t.Fatalf("taken: %d %v", status, body)
	}
	if got := h.rights.positionsOf("demo", "u1"); len(got) != 0 {
		t.Fatalf("u1 still holds %v in demo", got)
	}
	// In the other tenant, what that one names.
	if status, body := h.do(t, http.MethodPost, "/v1/tenants/solo/actions/give-position", checkoutOfShop, `{"person":"u1","position":"user"}`); status != http.StatusOK {
		t.Fatalf("given in solo: %d %v", status, body)
	}
	// A person the tenant has not is given nothing.
	if status, _ := h.do(t, http.MethodPost, givePosition, checkoutOfShop, `{"person":"nobody","position":"user"}`); status != http.StatusNotFound {
		t.Errorf("a person who is not the tenant's: %d, want 404", status)
	}
}

// Everything else is refused before the operator is asked: another
// position, another tenant, another component, an administrator's
// position, a role, no key, and a person's token, whoever's it is.
func TestAPositionGiverIsRefusedEverythingElse(t *testing.T) {
	f := newFakeIdentity("demo", "solo", "other")
	h := startWithIdentity(t, f)
	h.givers.Set(append(giversList(), registrar.PositionGiver{
		// A list written past the schema: an administrator's position.
		Key:   componentkeys.Key{Tenant: "shop", Component: "rogue", KeyHash: componentkeys.Hash("rogue-key")},
		Gives: []componentkeys.PositionGrant{{Tenant: "demo", Positions: []string{"tenant-administrator", "app-administrator"}}},
	}))
	tom := h.token(t, "tenant-demo", "tom")
	alice := h.token(t, "gentian", "alice")

	for name, c := range map[string]struct {
		path, key, body string
		want            int
	}{
		"a position the tenant does not name for it": {givePosition, checkoutOfShop, `{"person":"u1","position":"tenant-administrator"}`, http.StatusForbidden},
		"another position":                           {givePosition, checkoutOfShop, `{"person":"u1","position":"editor"}`, http.StatusForbidden},
		"another tenant":                             {"/v1/tenants/other/actions/give-position", checkoutOfShop, `{"person":"u1","position":"user"}`, http.StatusForbidden},
		"a position of another tenant's grant":       {"/v1/tenants/solo/actions/take-position", checkoutOfShop, `{"person":"u1","position":"sales-manager"}`, http.StatusForbidden},
		"another component":                          {givePosition, catalogOfShop, `{"person":"u1","position":"sales-manager"}`, http.StatusForbidden},
		"an administrator's, though listed":          {givePosition, "rogue-key", `{"person":"u1","position":"tenant-administrator"}`, http.StatusForbidden},
		"an app administrator's, though listed":      {takePosition, "rogue-key", `{"person":"u1","position":"app-administrator"}`, http.StatusForbidden},
		"a role":                                     {givePosition, checkoutOfShop, `{"person":"u1","position":"user","role":"admin"}`, http.StatusBadRequest},
		"no position":                                {givePosition, checkoutOfShop, `{"person":"u1"}`, http.StatusForbidden},
		"an unknown key":                             {givePosition, "not-a-key", `{"person":"u1","position":"user"}`, http.StatusForbidden},
		"no key":                                     {givePosition, "", `{"person":"u1","position":"user"}`, http.StatusUnauthorized},
		"a person who manages the people":            {givePosition, tom, `{"person":"u1","position":"user"}`, http.StatusForbidden},
		"a cluster administrator":                    {takePosition, alice, `{"person":"u1","position":"user"}`, http.StatusForbidden},
	} {
		if status, body := h.do(t, http.MethodPost, c.path, c.key, c.body); status != c.want {
			t.Errorf("%s: %d %v, want %d", name, status, body, c.want)
		}
	}
	if len(h.rights.asked) != 0 {
		t.Fatalf("the operator was asked %v", h.rights.asked)
	}
	if seen := f.realmsSeen(); len(seen) != 0 {
		t.Fatalf("a refused call reached %v", seen)
	}

	// The key opens nothing else: not the people's own routes for giving.
	if status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/actions/give", checkoutOfShop, `{"person":"u1","position":"user"}`); status != http.StatusUnauthorized {
		t.Errorf("the key at a person's route: %d, want 401", status)
	}
	if status, _ := h.do(t, http.MethodGet, "/v1/tenants/demo/people/u1/assignments", checkoutOfShop, ""); status != http.StatusUnauthorized {
		t.Errorf("the key reading assignments: %d, want 401", status)
	}

	// A tenant that stops naming the component: refused from then on.
	h.givers.Set(nil)
	if status, _ := h.do(t, http.MethodPost, givePosition, checkoutOfShop, `{"person":"u1","position":"user"}`); status != http.StatusForbidden {
		t.Errorf("a component no longer listed: %d, want 403", status)
	}
}
