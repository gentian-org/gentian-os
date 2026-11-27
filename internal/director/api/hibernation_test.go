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
	"time"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

func sleepRoute(tenant, action string) string {
	return "/v1/clusters/" + dt.Cluster + "/tenants/" + tenant + "/actions/" + action
}

func listedTenant(t *testing.T, h *harness, token, name string) map[string]any {
	t.Helper()
	_, body := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/tenants", token, "")
	for _, item := range body["tenants"].([]any) {
		if m := item.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("no tenant %s in the listing", name)
	return nil
}

// Putting a tenant to sleep and waking it is the cluster administrator's:
// each is one commit naming who asked, the state git already records is not
// a commit, the listing shows what holds, the platform tenant is refused and
// a tenant's own administrator holds nothing over it.
func TestATenantIsPutToSleepAndWokenByTheClusterAdministrator(t *testing.T) {
	h := start(t)
	alice := h.token(t, "gentian", "alice") // platform administrator

	code, out := h.do(t, "POST", sleepRoute("demo", "hibernate"), alice, "")
	if code != http.StatusAccepted || out["asleep"] != true || out["commit"] == nil {
		t.Fatalf("hibernate: %d %v", code, out)
	}
	if doc := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(doc, "  hibernation:\n    asleep: true") {
		t.Fatalf("the sleep is not in git:\n%s", doc)
	}
	if log := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main"); !strings.Contains(log, "put to sleep") || !strings.Contains(log, "alice") {
		t.Fatalf("the commit does not name who asked:\n%s", log)
	}
	if m := listedTenant(t, h, alice, "demo"); m["asleep"] != true {
		t.Fatalf("the listing says %v", m)
	}
	if code, out := h.do(t, "POST", sleepRoute("demo", "hibernate"), alice, ""); code != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("the same state again: %d %v", code, out)
	}

	code, out = h.do(t, "POST", sleepRoute("demo", "wake"), alice, `{"for":"5d"}`)
	if code != http.StatusAccepted || out["asleep"] != false {
		t.Fatalf("wake for five days: %d %v", code, out)
	}
	until, err := time.Parse(time.RFC3339, out["awakeUntil"].(string))
	if err != nil || time.Until(until) < 119*time.Hour || time.Until(until) > 121*time.Hour {
		t.Fatalf("awakeUntil = %v, %v", out["awakeUntil"], err)
	}
	if m := listedTenant(t, h, alice, "demo"); m["asleep"] != false || m["awakeUntil"] == nil {
		t.Fatalf("the listing says %v", m)
	}
	if doc := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(doc, "    asleep: true\n    awakeUntil: ") {
		t.Fatalf("the time is not in git:\n%s", doc)
	}

	for _, bad := range []string{`{"for":"soon"}`, `{"for":"-2d"}`, `{"for":"0h"}`, `{"for":"400d"}`, `{"until":"tomorrow"}`} {
		if code, _ := h.do(t, "POST", sleepRoute("demo", "wake"), alice, bad); code != http.StatusBadRequest {
			t.Fatalf("%s was accepted: %d", bad, code)
		}
	}

	if code, out := h.do(t, "POST", sleepRoute("demo", "wake"), alice, ""); code != http.StatusAccepted || out["asleep"] != false || out["awakeUntil"] != nil {
		t.Fatalf("wake: %d %v", code, out)
	}
	if m := listedTenant(t, h, alice, "demo"); m["asleep"] != false || m["awakeUntil"] != nil {
		t.Fatalf("the listing says %v", m)
	}
	if code, out := h.do(t, "POST", sleepRoute("demo", "wake"), alice, ""); code != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("waking an awake tenant: %d %v", code, out)
	}

	if code, _ := h.do(t, "POST", sleepRoute("platform", "hibernate"), alice, ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("the platform tenant was put to sleep: %d", code)
	}
	tina := h.token(t, "tenant-solo", "tina")
	for _, action := range []string{"hibernate", "wake"} {
		if code, _ := h.do(t, "POST", sleepRoute("demo", action), tina, ""); code != http.StatusForbidden {
			t.Fatalf("a tenant admin could %s a tenant: %d", action, code)
		}
	}
}
