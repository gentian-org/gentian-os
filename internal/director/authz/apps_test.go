/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package authz

import (
	"context"
	"sort"
	"testing"
)

// The rule the portal applied from a token's groups, as tuples: an app's own
// group entitles it; an addon is an app of its own; and a base with activated
// addons is entitled by the addons' groups and not by its own.
func TestAnAppIsEntitledByItsGroupAndABaseByItsAddons(t *testing.T) {
	want, err := appTuples("acme", []InstalledApp{
		{Profile: "xwiki-ce"},
		{Profile: "odoo-base-ce", Addons: []string{"crm-ce", "", "sales-ce"}},
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
		"group:gentian/tenant/acme/app/crm-ce#member|entitled|app:acme/crm-ce",
		"group:gentian/tenant/acme/app/crm-ce#member|entitled|app:acme/odoo-base-ce",
		"group:gentian/tenant/acme/app/sales-ce#member|entitled|app:acme/odoo-base-ce",
		"group:gentian/tenant/acme/app/sales-ce#member|entitled|app:acme/sales-ce",
		"group:gentian/tenant/acme/app/xwiki-ce#member|entitled|app:acme/xwiki-ce",
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

// Against the model: who may launch what, and that an app which is no longer
// installed leaves nothing behind.
func TestInstalledAppsAreLaunchedByTheirGroups(t *testing.T) {
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
	if err := c.ReconcileTenants(ctx, "apps-cluster", []string{tenant, "apps-b"}); err != nil {
		t.Fatal(err)
	}
	member := func(user, group string) Tuple {
		g, err := Group(group)
		if err != nil {
			t.Fatal(err)
		}
		return Tuple{User: "user:" + user, Relation: "member", Object: g}
	}
	if err := c.Write(ctx, []Tuple{
		member("wiki-reader", "gentian:tenant:apps-a:app:xwiki-ce"),
		member("base-only", "gentian:tenant:apps-a:app:odoo-base-ce"),
		member("crm-user", "gentian:tenant:apps-a:app:crm-ce"),
		member("boss", "gentian:tenant:apps-a:admins"),
		member("boss", "gentian:tenant:apps-a:app:xwiki-ce"),
		member("neighbour", "gentian:tenant:apps-b:app:xwiki-ce"),
	}, nil); err != nil {
		t.Fatal(err)
	}
	installed := map[string][]InstalledApp{
		tenant:   {{Profile: "xwiki-ce"}, {Profile: "odoo-base-ce", Addons: []string{"crm-ce"}}},
		"apps-b": nil,
	}
	if err := c.ReconcileApps(ctx, installed); err != nil {
		t.Fatal(err)
	}
	// A second pass writes nothing and changes nothing.
	if err := c.ReconcileApps(ctx, installed); err != nil {
		t.Fatal(err)
	}
	may := func(user, app string) bool {
		ok, err := c.Check(ctx, "test", "user:"+user, "can_launch", App(tenant, app))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for _, c := range []struct {
		user, app string
		want      bool
		why       string
	}{
		{"wiki-reader", "xwiki-ce", true, "a member of the app's group"},
		{"wiki-reader", "odoo-base-ce", false, "a member of another app's group"},
		{"crm-user", "crm-ce", true, "a member of the addon's group opens the addon"},
		{"crm-user", "odoo-base-ce", true, "and the base it lives in"},
		{"base-only", "odoo-base-ce", false, "the base's own group does not open a base that has addons"},
		{"boss", "xwiki-ce", false, "an administrator's account sees administration tiles only"},
		{"neighbour", "xwiki-ce", false, "the same app's group in another tenant"},
	} {
		if got := may(c.user, c.app); got != c.want {
			t.Errorf("%s on %s = %v, want %v: %s", c.user, c.app, got, c.want, c.why)
		}
	}

	// The addon is deactivated: the base is its own again, the addon is gone.
	installed[tenant] = []InstalledApp{{Profile: "xwiki-ce"}, {Profile: "odoo-base-ce"}}
	if err := c.ReconcileApps(ctx, installed); err != nil {
		t.Fatal(err)
	}
	if !may("base-only", "odoo-base-ce") || may("crm-user", "odoo-base-ce") || may("crm-user", "crm-ce") {
		t.Error("deactivating the addon did not return the base to its own group")
	}
	// Uninstalled: nothing is left to ask about.
	installed[tenant] = nil
	if err := c.ReconcileApps(ctx, installed); err != nil {
		t.Fatal(err)
	}
	if may("wiki-reader", "xwiki-ce") {
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
