/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// An add-on for everyone is stated on its entry in the add-ons' request,
// written under addonGrants beside the list, read back, and kept by a later
// request that does not mention it.
func TestAnAddonForEveryoneIsWrittenBesideTheList(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	h.asked.reset()
	code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom,
		`{"addons":[{"name":"calendar","defaultGrant":true},"deck"]}`)
	if code != http.StatusAccepted {
		t.Fatalf("set: %d %v", code, body)
	}
	if got := h.asked.questions(); len(got) != 2 || got[1] != "user:tom can_grant tenant:demo" {
		t.Fatalf("an add-on for everyone asked %q, want can_install_app then can_grant", got)
	}
	file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(file, "addonGrants:\n    - name: calendar\n      defaultGrant: true") || strings.Contains(file, "name: deck") {
		t.Fatalf("the grant is not beside the list:\n%s", file)
	}
	code, body = h.do(t, "GET", "/v1/tenants/demo/apps/nextcloud/addons", tom, "")
	if code != http.StatusOK || fmt.Sprint(body["addonGrants"]) != "[map[defaultGrant:true name:calendar]]" {
		t.Fatalf("get: %d %v", code, body)
	}

	// An add-on that says nothing keeps what it had, and one said to be
	// given per person writes nothing.
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom,
		`{"addons":["calendar",{"name":"deck","defaultGrant":false}]}`); code != http.StatusOK || body["status"] != "no_change" {
		t.Fatalf("a request that keeps the grant: %d %v", code, body)
	}
	file = dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(file, "- name: calendar\n      defaultGrant: true") || strings.Contains(file, "name: deck") {
		t.Fatalf("an unstated grant was not kept, or a per-person one was written:\n%s", file)
	}

	// An add-on that leaves the list takes its grant with it.
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["deck"]}`); code != http.StatusAccepted {
		t.Fatalf("remove: %d %v", code, body)
	}
	if file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); strings.Contains(file, "addonGrants") {
		t.Fatalf("the grant of an add-on switched off stayed:\n%s", file)
	}

	// A name and a build in one entry is not an add-on.
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom,
		`{"addons":[{"name":"deck","coordinate":"main/deck","digest":"sha256:`+strings.Repeat("a", 64)+`"}]}`); code != http.StatusBadRequest {
		t.Fatalf("name and build together: %d", code)
	}
}

// An add-on for everyone gives people access, so it needs can_grant as an
// install for everyone does; one given per person does not.
func TestAnAddonForEveryoneNeedsTheRightToGrant(t *testing.T) {
	h := startWith(t, nil, func(cfg *api.Config) { cfg.Authz = withoutGrant{cfg.Authz} })
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom,
		`{"addons":[{"name":"calendar","defaultGrant":true}]}`)
	if code != http.StatusForbidden {
		t.Fatalf("add-on for everyone without can_grant: %d %v", code, body)
	}
	if h.tip(t) != before {
		t.Fatal("a refused add-on moved the repository")
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom,
		`{"addons":[{"name":"calendar","defaultGrant":false}]}`); code != http.StatusAccepted {
		t.Fatalf("add-on per person: %d %v", code, body)
	}
}
