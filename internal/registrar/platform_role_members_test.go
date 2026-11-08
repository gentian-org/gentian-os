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

// Who holds each platform role through its group: the members of the group
// the claim names, read from the kernel realm and no other, for whoever may
// audit the cluster.
func TestThePlatformRoleMembersAreReadFromTheKernelRealm(t *testing.T) {
	f := newFakeIdentity("kernel", "demo")
	tenants := fakeTenants{"demo": ownRealm("demo"), "platform": {Name: "platform", Realm: "kernel"}}
	h := startWithTenants(t, f, nil, tenants, newFakeMailboxes())
	path := "/v1/clusters/" + dt.Cluster + "/platform-role-members"

	status, body := h.do(t, http.MethodGet, path, h.token(t, "gentian", "alice"), "")
	if status != http.StatusOK || body["complete"] != true {
		t.Fatalf("list: %d %v", status, body)
	}
	roles, _ := body["roles"].([]any)
	if len(roles) != 2 {
		t.Fatalf("roles: %v", roles)
	}
	admin := roles[0].(map[string]any)
	if admin["role"] != "admin" || admin["group"] != platformAdmins {
		t.Fatalf("first role: %v", admin)
	}
	if members, ok := admin["members"].([]any); !ok || len(members) != 0 {
		t.Fatalf("an empty group is listed as one, not as unread: %v", admin)
	}
	for _, realm := range f.realmsSeen() {
		if realm != "kernel" {
			t.Fatalf("realm %q was reached", realm)
		}
	}

	if status, _ := h.do(t, http.MethodGet, path, h.token(t, "tenant-demo", "tom"), ""); status != http.StatusForbidden {
		t.Fatalf("a tenant administrator listing the platform roles' members: %d, want 403", status)
	}

	// Without the kernel realm's credential, every role is unread and the
	// answer says it is incomplete.
	h = startWithTenants(t, newFakeIdentity("demo"), nil, tenants, newFakeMailboxes())
	status, body = h.do(t, http.MethodGet, path, h.token(t, "gentian", "alice"), "")
	if status != http.StatusOK || body["complete"] != false {
		t.Fatalf("without the kernel realm: %d %v", status, body)
	}
	if first := body["roles"].([]any)[0].(map[string]any); first["unavailable"] != true || first["members"] != nil {
		t.Fatalf("an unread role: %v", first)
	}
}
