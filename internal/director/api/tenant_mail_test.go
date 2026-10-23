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

func mailModeRoute(tenant string) string {
	return "/v1/clusters/" + dt.Cluster + "/tenants/" + tenant + "/mail-mode"
}

// A tenant's mail mode is the cluster administrator's to set: one commit of
// the manifest, read back on the listing. A word the Tenant kind does not
// know is refused, and a tenant's own administrator holds nothing over it.
func TestATenantsMailModeIsSetByTheClusterAdministrator(t *testing.T) {
	h := start(t)
	alice := h.token(t, "gentian", "alice") // platform administrator

	if code, out := h.do(t, "PUT", mailModeRoute("demo"), alice, `{"mode":"disabled"}`); code != http.StatusAccepted {
		t.Fatalf("set: %d %v", code, out)
	}
	if doc := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(doc, "    mode: disabled") {
		t.Fatalf("the mode is not in git:\n%s", doc)
	}
	_, body := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/tenants", alice, "")
	found := ""
	for _, item := range body["tenants"].([]any) {
		m := item.(map[string]any)
		if m["name"] == "demo" {
			found, _ = m["mailMode"].(string)
		}
	}
	if found != "disabled" {
		t.Fatalf("the listing says mailMode=%q", found)
	}
	if code, out := h.do(t, "PUT", mailModeRoute("demo"), alice, `{"mode":"disabled"}`); code != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("the same mode again: %d %v", code, out)
	}
	if code, _ := h.do(t, "PUT", mailModeRoute("demo"), alice, `{"mode":"carrier-pigeon"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown mode was accepted: %d", code)
	}
	if code, _ := h.do(t, "PUT", mailModeRoute("demo"), alice, `{}`); code != http.StatusBadRequest {
		t.Fatalf("an empty body was accepted: %d", code)
	}
	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "PUT", mailModeRoute("demo"), tina, `{"mode":"disabled"}`); code != http.StatusForbidden {
		t.Fatalf("a tenant admin set a tenant's mail mode: %d", code)
	}
}
