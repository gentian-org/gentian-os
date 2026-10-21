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
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/registrar"
)

// Who could be made a security officer: each tenant's administrators, read
// from each tenant's own realm, for whoever configures the cluster. The
// platform tenant and a tenant that adopts another realm are left out; a
// realm that cannot be read is listed as such and the answer says it is
// incomplete.
func TestTheTenantsAdministratorsAreListedForWhoeverConfiguresTheCluster(t *testing.T) {
	f := newFakeIdentity("demo", "solo") // other deliberately absent
	tenants := fakeTenants{
		"demo": ownRealm("demo"), "solo": ownRealm("solo"), "other": ownRealm("other"),
		"platform": {Name: "platform", Realm: "kernel"},
	}
	h := startWithTenants(t, f, nil, tenants, newFakeMailboxes())
	path := "/v1/clusters/" + dt.Cluster + "/tenant-administrators"

	status, body := h.do(t, http.MethodGet, path, h.token(t, "gentian", "alice"), "")
	if status != http.StatusOK || body["complete"] != false {
		t.Fatalf("list: %d %v", status, body)
	}
	byTenant := map[string]map[string]any{}
	for _, raw := range body["tenants"].([]any) {
		entry := raw.(map[string]any)
		byTenant[entry["tenant"].(string)] = entry
	}
	if _, listed := byTenant["platform"]; listed || len(byTenant) != 3 {
		t.Fatalf("tenants listed: %v", byTenant)
	}
	demo := byTenant["demo"]
	admins, _ := demo["administrators"].([]any)
	if len(admins) != 1 || admins[0].(map[string]any)["username"] != "admin@demo.example" || admins[0].(map[string]any)["id"] != "a1" {
		t.Fatalf("demo's administrators: %v", demo)
	}
	if other := byTenant["other"]; other["unavailable"] != true || other["administrators"] != nil {
		t.Fatalf("a realm with no credential: %v", other)
	}
	// Read from the admins group of each realm, and nowhere else.
	if got := f.lastGroup; got != "gentian:tenant:solo:admins" && got != "gentian:tenant:demo:admins" {
		t.Fatalf("the group read: %q", got)
	}
	for _, realm := range f.realmsSeen() {
		if realm != "demo" && realm != "solo" {
			t.Fatalf("realm %q was reached", realm)
		}
	}

	// A tenant's administrator, and the security officer, may not list them:
	// the relation is the one the choice is written under.
	if status, _ := h.do(t, http.MethodGet, path, h.token(t, "tenant-demo", "tom"), ""); status != http.StatusForbidden {
		t.Fatalf("a tenant administrator listing every tenant's administrators: %d, want 403", status)
	}
	var _ registrar.Tenant // the type the fake stands in for
}
