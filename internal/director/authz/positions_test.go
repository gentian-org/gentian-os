/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"sort"
	"strings"
	"testing"
)

// The four positions every tenant has follow from what it has installed:
// User holds the tenant and no app, All apps includes User and holds every
// app and add-on, App administrator every app that is not an add-on, Tenant
// administrator the tenant and no app. An app given to user, or once given
// to everyone, gives User nothing.
func TestThePositionsEveryTenantHas(t *testing.T) {
	got, err := positionTuples("acme", ShippedPositions([]InstalledApp{
		{Profile: "wiki", Positions: []string{PositionUser}},
		{Profile: "shop", Addons: []InstalledApp{{Profile: "shop-sales"}, {Profile: "shop-stock"}}},
		{Profile: ""},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{
		"position:acme/all-apps#holder|user|app:acme/shop",
		"position:acme/all-apps#holder|user|app:acme/shop-sales",
		"position:acme/all-apps#holder|user|app:acme/shop-stock",
		"position:acme/all-apps#holder|user|app:acme/wiki",
		"position:acme/all-apps|included_in|position:acme/user",
		"position:acme/app-administrator#holder|admin|app:acme/shop",
		"position:acme/app-administrator#holder|admin|app:acme/wiki",
		"position:acme/tenant-administrator#holder|admin|tenant:acme",
		"position:acme/user#holder|user|tenant:acme",
		"tenant:acme|tenant|position:acme/all-apps",
		"tenant:acme|tenant|position:acme/app-administrator",
		"tenant:acme|tenant|position:acme/tenant-administrator",
		"tenant:acme|tenant|position:acme/user",
	}
	if strings.Join(keys, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tuples:\n%s\nwant:\n%s", strings.Join(keys, "\n"), strings.Join(want, "\n"))
	}
	for _, name := range []string{PositionUser, PositionAllApps, PositionAppAdministrator, PositionTenantAdministrator} {
		if !Shipped(name) {
			t.Errorf("%s is not told from a tenant's own", name)
		}
	}
	if Shipped("sales-manager") {
		t.Error("a tenant's own position is taken for one every tenant has")
	}
}

// All apps follows the installed set: with nothing installed it holds no
// app and still includes User.
func TestAllAppsFollowsWhatIsInstalled(t *testing.T) {
	got, err := positionTuples("acme", ShippedPositions(nil))
	if err != nil {
		t.Fatal(err)
	}
	for k := range got {
		if strings.HasPrefix(k, "position:acme/all-apps#holder|") || strings.HasPrefix(k, "position:acme/user#holder|user|app:") {
			t.Errorf("unexpected %s", k)
		}
	}
	if _, ok := got["position:acme/all-apps|included_in|position:acme/user"]; !ok {
		t.Error("all-apps does not include user")
	}
}

// A tenant cannot declare a position under a name every tenant has.
func TestATenantCannotDeclareAShippedPosition(t *testing.T) {
	for _, name := range []string{PositionUser, PositionAllApps, PositionAppAdministrator, PositionTenantAdministrator} {
		err := ValidatePositions("acme", []PositionSpec{{Name: name}})
		if err == nil || !strings.Contains(err.Error(), "every tenant has") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := ValidatePositions("acme", []PositionSpec{{Name: "sales", Includes: []string{PositionAllApps}}}); err != nil {
		t.Errorf("a tenant's own position may include all-apps: %v", err)
	}
}

// A declared position: the roles it holds, on the tenant, on an app and of
// the ones an app declares, and the positions it includes.
func TestADeclaredPositionIsItsRolesAndWhatItIncludes(t *testing.T) {
	got, err := positionTuples("acme", []PositionSpec{
		{Name: "sales-manager", Roles: []RoleGrant{{Role: RoleUser}, {App: "shop", Role: RoleUser}, {App: "shop-sales", Role: "manager"}}},
		{Name: "executive", Includes: []string{"sales-manager"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tenant:acme|tenant|position:acme/sales-manager",
		"tenant:acme|tenant|position:acme/executive",
		"position:acme/sales-manager#holder|user|tenant:acme",
		"position:acme/sales-manager#holder|user|app:acme/shop",
		"position:acme/sales-manager#holder|holder|role:acme/shop-sales/manager",
		"position:acme/executive|included_in|position:acme/sales-manager",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	if len(got) != 6 {
		t.Errorf("tuples = %d, want 6", len(got))
	}
}

// A declaration the store cannot hold is refused whole, and says why.
func TestADeclarationThatCannotBeHeldIsRefused(t *testing.T) {
	for name, c := range map[string]struct {
		positions []PositionSpec
		says      string
	}{
		"a name that is none":            {[]PositionSpec{{Name: "Sales Manager"}}, "not a position's name"},
		"declared twice":                 {[]PositionSpec{{Name: "a"}, {Name: "a"}}, "declared twice"},
		"includes one nobody declared":   {[]PositionSpec{{Name: "a", Includes: []string{"b"}}}, "not a position of the tenant"},
		"includes itself":                {[]PositionSpec{{Name: "a", Includes: []string{"a"}}}, "includes itself"},
		"a circle":                       {[]PositionSpec{{Name: "a", Includes: []string{"b"}}, {Name: "b", Includes: []string{"c"}}, {Name: "c", Includes: []string{"a"}}}, "a includes b includes c includes a"},
		"a tenant role that is none":     {[]PositionSpec{{Name: "a", Roles: []RoleGrant{{Role: "manager"}}}}, "user and admin"},
		"an app that is no name":         {[]PositionSpec{{Name: "a", Roles: []RoleGrant{{App: "x/y", Role: RoleUser}}}}, "not an app's name"},
		"a declared role that is none":   {[]PositionSpec{{Name: "a", Roles: []RoleGrant{{App: "shop", Role: "Sales#1"}}}}, "not a role's name"},
		"a role on another tenant's app": {[]PositionSpec{{Name: "a", Roles: []RoleGrant{{App: "../other", Role: RoleUser}}}}, "not an app's name"},
	} {
		_, err := positionTuples("acme", c.positions)
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, c.says)
		}
	}
	// Two positions that include the same third are no circle.
	if _, err := positionTuples("acme", []PositionSpec{
		{Name: "a", Includes: []string{"c"}}, {Name: "b", Includes: []string{"c"}}, {Name: "c"},
	}); err != nil {
		t.Errorf("a shared inclusion is refused: %v", err)
	}
}

func TestNamesOfPositionsAndRoles(t *testing.T) {
	for name, ok := range map[string]bool{
		"sales-manager": true, "a": true, "a1": true,
		"": false, "-a": false, "a-": false, "Sales": false, "a/b": false, "a#b": false, "a:b": false, "a b": false,
		strings.Repeat("a", 63): true, strings.Repeat("a", 64): false,
	} {
		if ValidName(name) != ok {
			t.Errorf("ValidName(%q) = %v", name, !ok)
		}
	}
	if Position("acme", "leads") != "position:acme/leads" || Role("acme", "shop", "manager") != "role:acme/shop/manager" ||
		Holders(Position("acme", "leads")) != "position:acme/leads#holder" {
		t.Error("the objects are not named as the model names them")
	}
}

// An app given to positions: each named position holds its role user, a
// tenant's own beside the roles it declares, and a name that is no position
// gives nothing, nor does user.
func TestAnAppGivenToPositions(t *testing.T) {
	apps := []InstalledApp{
		{Profile: "wiki", Positions: []string{PositionUser}},
		{Profile: "shop", Positions: []string{PositionTenantAdministrator, "sales", "gone"}},
	}
	own := WithAppsGivenTo(PositionSpec{Name: "sales", Roles: []RoleGrant{{App: "shop", Role: RoleUser}, {App: "wiki", Role: RoleAdmin}}}, apps)
	if len(own.Roles) != 2 {
		t.Fatalf("a role the position already declares was added again: %v", own.Roles)
	}
	plain := WithAppsGivenTo(PositionSpec{Name: "sales"}, apps)
	if len(plain.Roles) != 1 || plain.Roles[0] != (RoleGrant{App: "shop", Role: RoleUser}) {
		t.Fatalf("the position does not hold the app it was given: %v", plain.Roles)
	}
	got, err := positionTuples("acme", append(ShippedPositions(apps), plain))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"position:acme/tenant-administrator#holder|user|app:acme/shop",
		"position:acme/sales#holder|user|app:acme/shop",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	for k := range got {
		if strings.Contains(k, "gone") || strings.HasPrefix(k, "position:acme/user#holder|user|app:") {
			t.Errorf("unexpected %s", k)
		}
	}
}
