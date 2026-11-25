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
	"reflect"
	"testing"
)

// What an app is handed is each person's roles on it, as a check would
// answer them: through a position and the positions it includes, through a
// role given directly, through the base's administrator; the app's
// administrator holds user and every declared role. Positions are not
// handed over, and nobody who holds nothing is named.
func TestAnAppIsHandedEachPersonsRolesOnIt(t *testing.T) {
	ctx := context.Background()
	s := tenantStore(t, "demo")
	more, err := positionTuples("demo", []PositionSpec{
		{Name: "sellers", Roles: []RoleGrant{{App: "shop-sales", Role: "manager"}}},
		{Name: "lead", Includes: []string{"sellers"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tuple := range more {
		s.memoryStore[tuple] = true
	}
	for sub, a := range map[string]Assignment{
		"anna":        {App: "shop-sales", Role: "manager"},
		"bert":        {Position: "lead"},
		"cleo":        {App: "shop", Role: "admin"},
		"f:google:42": {App: "shop-sales", Role: "user"},
		"dora":        {Position: "chief"},
		"emil":        {App: "wiki", Role: "user"},
	} {
		if _, err := Give(ctx, s, "demo", sub, a); err != nil {
			t.Fatalf("give %s: %v", sub, err)
		}
	}
	if err := ReconcileDerived(ctx, s, []string{"demo"}); err != nil {
		t.Fatal(err)
	}

	got, err := AppRoleHoldings(ctx, s, "demo", []string{"shop-sales", "shop", "notes"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]RoleHolding{
		"shop-sales": {
			{Subject: "anna", Roles: []string{"manager", "user"}},
			{Subject: "bert", Roles: []string{"manager", "user"}},
			{Subject: "cleo", Roles: []string{"admin", "manager", "user"}},
			{Subject: "f:google:42", Roles: []string{"user"}},
		},
		"shop": {
			{Subject: "anna", Roles: []string{"user"}},
			{Subject: "bert", Roles: []string{"user"}},
			{Subject: "cleo", Roles: []string{"admin", "user"}},
			{Subject: "f:google:42", Roles: []string{"user"}},
		},
		"notes": {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("holdings:\n%+v\nwant\n%+v", got, want)
	}
}
