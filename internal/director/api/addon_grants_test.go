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

// An add-on entry may carry defaultGrant, which is read and has no effect:
// nothing more is asked, nothing is written beside the list, and a leftover
// addonGrants block goes with the next rewrite of the add-ons.
func TestAnAddonsDefaultGrantHasNoEffect(t *testing.T) {
	h := startWith(t, nil, func(cfg *api.Config) { cfg.Authz = withoutGrant{cfg.Authz} })
	tom := h.token(t, "tenant-demo", "tom")

	h.asked.reset()
	code, body := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom,
		`{"addons":[{"name":"calendar","defaultGrant":true},"deck"]}`)
	if code != http.StatusAccepted {
		t.Fatalf("set: %d %v", code, body)
	}
	if got := h.asked.questions(); len(got) != 1 || got[0] != "user:tom can_install_app tenant:demo" {
		t.Fatalf("setting add-ons asked %q, want can_install_app alone", got)
	}
	file := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if strings.Contains(file, "addonGrants") || strings.Contains(file, "defaultGrant") {
		t.Fatalf("a grant was written:\n%s", file)
	}
	code, body = h.do(t, "GET", "/v1/tenants/demo/apps/nextcloud/addons", tom, "")
	if code != http.StatusOK || fmt.Sprint(body["addonGrants"]) != "[]" || fmt.Sprint(body["addons"]) != "[calendar deck]" {
		t.Fatalf("get: %d %v", code, body)
	}

	// A name and a build in one entry is not an add-on.
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom,
		`{"addons":[{"name":"deck","coordinate":"main/deck","digest":"sha256:`+strings.Repeat("a", 64)+`"}]}`); code != http.StatusBadRequest {
		t.Fatalf("name and build together: %d", code)
	}
}
