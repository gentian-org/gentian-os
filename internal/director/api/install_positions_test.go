/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"net/http"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// An install names the positions the app is given to. User is written as
// the default grant, the others as a list on the entry, in one commit; the
// same request on an installed app replaces what the entry said.
func TestAnAppIsInstalledForPositions(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	if code, body := h.do(t, "PUT", "/v1/tenants/demo/positions/sales", tom, `{"title":"Sales"}`); code >= 300 {
		t.Fatalf("position: %d %v", code, body)
	}
	h.asked.reset()
	code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom,
		`{"positions":["user","sales","tenant-administrator","sales"]}`)
	if code != http.StatusAccepted {
		t.Fatalf("install: %d %v", code, body)
	}
	if got := h.asked.questions(); len(got) != 2 || got[1] != "user:tom can_grant tenant:demo" {
		t.Fatalf("an install for positions asked %q, want can_install_app then can_grant", got)
	}
	file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(file, "  - profile: element\n    defaultGrant: true\n    positions: [sales, tenant-administrator]\n") {
		t.Fatalf("the entry does not say who has the app:\n%s", file)
	}

	// Again with fewer: user leaves, and so does the default grant.
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, `{"positions":["sales"]}`); code != http.StatusAccepted || body["status"] != "updated" {
		t.Fatalf("change: %d %v", code, body)
	}
	file = dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(file, "  - profile: element\n    positions: [sales]\n") || strings.Contains(file, "defaultGrant") {
		t.Fatalf("the entry was not replaced:\n%s", file)
	}

	// Nobody: the key goes. A request that names no positions leaves it.
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, `{"positions":[]}`); code != http.StatusAccepted {
		t.Fatalf("none: %d %v", code, body)
	}
	if file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); strings.Contains(file, "positions:") {
		t.Fatalf("an app given to nobody still names positions:\n%s", file)
	}

	// A name that is no position of the tenant is refused, nothing written.
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, `{"positions":["nobody-has-this"]}`); code != http.StatusBadRequest {
		t.Fatalf("unknown position: %d", code)
	}
}
