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

// What installed apps are in the store: each attached to its tenant, an
// add-on an app of its own tied to its base both ways, and every declared
// role tied to its app. Nothing says who may use one.
func TestInstalledAppsAreAttachedWithTheirAddonsAndRoles(t *testing.T) {
	want, err := appTuples("acme", []InstalledApp{
		{Profile: "xwiki-ce"},
		{Profile: "odoo-base-ce", Addons: []InstalledApp{
			{Profile: "crm-ce"}, {Profile: ""},
			{Profile: "sales-ce", Roles: []DeclaredRole{{Name: "manager", Includes: []string{"salesman"}}, {Name: "salesman"}}},
		}},
		{Profile: ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range want {
		got = append(got, k)
	}
	sort.Strings(got)
	expect := []string{
		"app:acme/crm-ce|addon|app:acme/odoo-base-ce",
		"app:acme/odoo-base-ce|base|app:acme/crm-ce",
		"app:acme/odoo-base-ce|base|app:acme/sales-ce",
		"app:acme/sales-ce|addon|app:acme/odoo-base-ce",
		"app:acme/sales-ce|app|role:acme/sales-ce/manager",
		"app:acme/sales-ce|app|role:acme/sales-ce/salesman",
		"role:acme/sales-ce/manager|declared|app:acme/sales-ce",
		"role:acme/sales-ce/manager|included_in|role:acme/sales-ce/salesman",
		"role:acme/sales-ce/salesman|declared|app:acme/sales-ce",
		"tenant:acme|tenant|app:acme/crm-ce",
		"tenant:acme|tenant|app:acme/odoo-base-ce",
		"tenant:acme|tenant|app:acme/sales-ce",
		"tenant:acme|tenant|app:acme/xwiki-ce",
	}
	if len(got) != len(expect) {
		t.Fatalf("tuples:\n%v\nwant:\n%v", got, expect)
	}
	for i := range got {
		if got[i] != expect[i] {
			t.Fatalf("tuple %d = %q, want %q", i, got[i], expect[i])
		}
	}
}

// A declaration the store cannot hold is refused, naming what is wrong.
func TestAnAppsRolesThatCannotBeHeldAreRefused(t *testing.T) {
	for name, apps := range map[string][]InstalledApp{
		"a role named as one every app has": {{Profile: "shop", Roles: []DeclaredRole{{Name: "admin"}}}},
		"a role that is no name":            {{Profile: "shop", Roles: []DeclaredRole{{Name: "Sales Manager"}}}},
		"a role including one not declared": {{Profile: "shop", Roles: []DeclaredRole{{Name: "manager", Includes: []string{"ghost"}}}}},
		"a role including itself":           {{Profile: "shop", Roles: []DeclaredRole{{Name: "manager", Includes: []string{"manager"}}}}},
		"an app that is no name":            {{Profile: "shop/one"}},
	} {
		if _, err := appTuples("acme", apps); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

// Against the model: who may launch what, and that an app which is no longer
// installed leaves nothing behind.
func TestInstalledAppsAreLaunchedByWhoHoldsARoleOnThem(t *testing.T) {
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
	const tenant = "apps-a"
	if err := c.ReconcileTenants(ctx, "apps-cluster", TenantsNamed(tenant, "apps-b")); err != nil {
		t.Fatal(err)
	}
	sales := InstalledApp{Profile: "crm-ce", Roles: []DeclaredRole{{Name: "manager", Includes: []string{"salesman"}}, {Name: "salesman"}}}
	installed := map[string][]InstalledApp{
		tenant:   {{Profile: "xwiki-ce", ForEveryone: true}, {Profile: "odoo-base-ce", Addons: []InstalledApp{sales}}},
		"apps-b": {{Profile: "xwiki-ce"}},
	}
	if err := c.ReconcileApps(ctx, installed); err != nil {
		t.Fatal(err)
	}
	positions := map[string][]PositionSpec{
		tenant: append(ShippedPositions(installed[tenant]),
			PositionSpec{Name: "sales-manager", Roles: []RoleGrant{{Role: RoleUser}, {App: "crm-ce", Role: "manager"}}},
			PositionSpec{Name: "executive", Includes: []string{"sales-manager"}}),
		"apps-b": ShippedPositions(installed["apps-b"]),
	}
	if err := c.ReconcilePositions(ctx, positions); err != nil {
		t.Fatal(err)
	}
	holds := func(user, position string) Tuple {
		return Tuple{User: "user:" + user, Relation: "holder", Object: Position(tenant, position)}
	}
	if err := c.Write(ctx, []Tuple{
		holds("reader", PositionUser),
		holds("seller", "sales-manager"),
		holds("chief", "executive"),
		holds("boss", PositionTenantAdministrator),
		holds("keeper", PositionAppAdministrator),
		{User: "user:boss", Relation: "user", Object: App(tenant, "xwiki-ce")},
		{User: "user:neighbour", Relation: "holder", Object: Position("apps-b", PositionUser)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// A second pass writes nothing and changes nothing.
	if err := c.ReconcileApps(ctx, installed); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcilePositions(ctx, positions); err != nil {
		t.Fatal(err)
	}
	ask := func(user, relation, object string) bool {
		ok, err := c.Check(ctx, "test", "user:"+user, relation, object)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	may := func(user, app string) bool { return ask(user, "can_launch", App(tenant, app)) }
	for _, c := range []struct {
		user, app string
		want      bool
		why       string
	}{
		{"reader", "xwiki-ce", true, "an app installed for everyone is the position User's"},
		{"reader", "odoo-base-ce", false, "an app that is not, is not"},
		{"seller", "crm-ce", true, "the position holds a role the add-on declares"},
		{"seller", "odoo-base-ce", true, "and a role on an add-on lets a person into its base"},
		{"chief", "crm-ce", true, "a position that includes another holds its roles"},
		{"boss", "xwiki-ce", true, "an administrator uses an app they were given"},
		{"boss", "odoo-base-ce", false, "and no app they were not: a tenant's admin holds no role on its apps"},
		{"keeper", "crm-ce", true, "the admin of a base administers and uses its add-on"},
		{"neighbour", "xwiki-ce", false, "a person of another tenant"},
	} {
		if got := may(c.user, c.app); got != c.want {
			t.Errorf("%s on %s = %v, want %v: %s", c.user, c.app, got, c.want, c.why)
		}
	}
	if !ask("keeper", "can_administer", App(tenant, "crm-ce")) || ask("seller", "can_administer", App(tenant, "crm-ce")) {
		t.Error("an add-on is administered by its base's admin, and by nobody for holding a role on it")
	}
	if !ask("seller", "holder", Role(tenant, "crm-ce", "salesman")) {
		t.Error("manager includes salesman")
	}
	roles, err := c.AppRoles(ctx, "test", tenant, "crm-ce", "chief")
	if err != nil || strings.Join(roles, ",") != "manager,salesman,user" {
		t.Errorf("what the add-on is told the executive is = %v, %v", roles, err)
	}

	// The position is no longer declared: its holders go with it, and with
	// them what it held.
	positions[tenant] = ShippedPositions(installed[tenant])
	if err := c.ReconcilePositions(ctx, positions); err == nil {
		// executive includes a position that is gone: refused whole, and
		// nothing of the tenant's is changed.
		if may("seller", "crm-ce") {
			t.Error("a position that is no longer declared still opens an app")
		}
	}
	if left, err := c.Read(ctx, Tuple{Relation: "holder", Object: Position(tenant, "sales-manager")}); err != nil || len(left) != 0 {
		t.Errorf("holders left of a position that is gone: %v, %v", left, err)
	}
	if !may("reader", "xwiki-ce") {
		t.Error("removing one position took another's holders")
	}

	// The add-on is deactivated: its roles and their holders go, the base
	// stays.
	installed[tenant] = []InstalledApp{{Profile: "xwiki-ce", ForEveryone: true}, {Profile: "odoo-base-ce"}}
	if err := c.ReconcileApps(ctx, installed); err != nil {
		t.Fatal(err)
	}
	if may("keeper", "crm-ce") || !may("keeper", "odoo-base-ce") {
		t.Error("deactivating the add-on did not leave the base as it was")
	}
	// Uninstalled: nothing is left to ask about.
	installed[tenant] = nil
	if err := c.ReconcileApps(ctx, installed); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcilePositions(ctx, map[string][]PositionSpec{tenant: ShippedPositions(nil)}); err != nil {
		t.Fatal(err)
	}
	if may("reader", "xwiki-ce") {
		t.Error("an uninstalled app can still be launched")
	}
	left, err := c.Read(ctx, Tuple{User: Tenant(tenant), Relation: "tenant", Object: "app:"})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("tuples left of uninstalled apps: %v", left)
	}
}
