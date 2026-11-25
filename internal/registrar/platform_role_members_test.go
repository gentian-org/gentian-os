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
)

// Who of the platform tenant holds each cluster role: the holders the claim
// names, each as the kernel realm knows them and read from no other realm,
// for whoever may audit the cluster.
func TestThePlatformTenantsRoleHoldersAreReadFromTheKernelRealm(t *testing.T) {
	f := newFakeIdentity("kernel", "demo")
	tenants := fakeTenants{"demo": ownRealm("demo"), "platform": {Name: "platform", Realm: "kernel"}}
	h := startWithTenants(t, f, nil, tenants, newFakeMailboxes())
	path := "/v1/clusters/" + dt.Cluster + "/platform-role-members"

	status, body := h.do(t, http.MethodGet, path, h.token(t, "gentian", "alice"), "")
	if status != http.StatusOK || body["complete"] != false {
		t.Fatalf("list: %d %v, want an answer that says one holder was not found", status, body)
	}
	roles, _ := body["roles"].([]any)
	if len(roles) != 2 {
		t.Fatalf("roles: %v, want admin and securityOfficer: another tenant's person is not the platform tenant's", roles)
	}
	admin := roles[0].(map[string]any)
	members, _ := admin["members"].([]any)
	if admin["role"] != "admin" || len(members) != 2 || admin["unavailable"] != true {
		t.Fatalf("first role: %v", admin)
	}
	if _, has := admin["group"]; has {
		t.Fatalf("a role is held by name, not through a group: %v", admin)
	}
	if known := members[0].(map[string]any); known["id"] != "u1" || known["username"] != "ada@example.com" {
		t.Fatalf("a holder the realm knows: %v", known)
	}
	if former := members[1].(map[string]any); former["id"] != "gone" || former["username"] != "former@example.com" {
		t.Fatalf("a holder the realm no longer knows is listed as the claim names them: %v", former)
	}
	officer := roles[1].(map[string]any)
	if officer["role"] != "securityOfficer" || officer["unavailable"] == true {
		t.Fatalf("second role: %v", officer)
	}
	for _, realm := range f.realmsSeen() {
		if realm != "kernel" {
			t.Fatalf("realm %q was reached", realm)
		}
	}

	if status, _ := h.do(t, http.MethodGet, path, h.token(t, "tenant-demo", "tom"), ""); status != http.StatusForbidden {
		t.Fatalf("a tenant administrator listing the cluster's role holders: %d, want 403", status)
	}

	// Without the kernel realm's credential the holders are listed as the
	// claim names them and the answer says it is incomplete.
	h = startWithTenants(t, newFakeIdentity("demo"), nil, tenants, newFakeMailboxes())
	status, body = h.do(t, http.MethodGet, path, h.token(t, "gentian", "alice"), "")
	if status != http.StatusOK || body["complete"] != false {
		t.Fatalf("without the kernel realm: %d %v", status, body)
	}
	first := body["roles"].([]any)[0].(map[string]any)
	if members, _ := first["members"].([]any); first["unavailable"] != true || len(members) != 2 {
		t.Fatalf("an unread role: %v", first)
	}
}
