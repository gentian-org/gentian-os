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

// The store's sign-in is the tenant administrator's to turn on and off, with
// an https issuer; a member may read it and change nothing.
func TestTheStoreSignInIsTheTenantAdministrators(t *testing.T) {
	h := startWith(t, nil)
	tom := h.token(t, "tenant-demo", "tom") // demo's administrator
	mia := h.token(t, "tenant-demo", "mia") // a member
	path := "/v1/tenants/demo/store-sign-in"
	issuer := "https://id.store.example/auth/realms/customers"

	code, body := h.do(t, "GET", path, mia, "")
	if code != http.StatusOK || body["enabled"] != false {
		t.Fatalf("read before: %d %v", code, body)
	}
	before := h.tip(t)
	if code, _ := h.do(t, "PUT", path, mia, `{"issuer":"`+issuer+`"}`); code != http.StatusForbidden || h.tip(t) != before {
		t.Fatalf("a member turned it on: %d", code)
	}
	for _, bad := range []string{"http://id.store.example/auth/realms/c", "https://id.store.example:8443/auth/realms/c", "https://id.store.example/auth/realms/c/"} {
		if code, body := h.do(t, "PUT", path, tom, `{"issuer":"`+bad+`"}`); code != http.StatusUnprocessableEntity || h.tip(t) != before {
			t.Fatalf("%s: %d %v", bad, code, body)
		}
	}
	if code, body := h.do(t, "PUT", path, tom, `{"issuer":"`+issuer+`"}`); code != http.StatusAccepted {
		t.Fatalf("turn on: %d %v", code, body)
	}
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(manifest, "  store:\n    signIn:\n      issuer: \""+issuer+"\"") {
		t.Fatalf("the manifest does not carry the issuer:\n%s", manifest)
	}
	if code, body := h.do(t, "GET", path, mia, ""); code != http.StatusOK || body["issuer"] != issuer {
		t.Fatalf("read after: %d %v", code, body)
	}
	if code, body := h.do(t, "DELETE", path, tom, ""); code != http.StatusAccepted {
		t.Fatalf("turn off: %d %v", code, body)
	}
	if strings.Contains(dt.RemoteFile(t, h.remote, dt.TenantPath("demo")), "signIn:") {
		t.Fatal("the switch is still in the manifest")
	}
}
