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
	"reflect"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// A role holder is an account of a tenant's realm the claim names -- of a
// tenant other than the platform's, one of its administrators -- written by
// the cluster's administrator alone with a reason, read by whoever may
// audit, and taken away by the same commit path.
func TestARoleHolderIsDeclaredOnTheClaimByTheClustersAdministrator(t *testing.T) {
	h := startWith(t, nil)
	alice := h.token(t, "gentian", "alice") // the cluster's administrator
	tom := h.token(t, "tenant-demo", "tom") // demo's administrator
	base := "/v1/clusters/" + dt.Cluster + "/platform-roles/holders"

	code, body := h.do(t, "GET", base, alice, "")
	if code != http.StatusOK {
		t.Fatalf("read: %d %v", code, body)
	}
	if list, _ := body["roleHolders"].([]any); len(list) != 0 {
		t.Fatalf("holders before anybody was named: %v", list)
	}
	if roles, _ := body["roles"].([]any); fmt.Sprint(roles) != "[auditor breakGlass perimeterApprover securityOfficer serviceAdmin sharedAppsAdmin tenantAuditor]" {
		t.Fatalf("the roles a holder may hold: %v", body["roles"])
	}

	before := h.tip(t)
	if code, _ := h.do(t, "PUT", base+"/securityOfficer/demo/7d0f3c2e", tom, `{"username":"tom@demo.example","reason":"r"}`); code != http.StatusForbidden || h.tip(t) != before {
		t.Fatalf("a tenant's administrator named a holder: %d", code)
	}
	for _, b := range []string{`{}`, `{"username":"it@demo.example"}`} {
		if code, body := h.do(t, "PUT", base+"/securityOfficer/demo/7d0f3c2e", alice, b); code != http.StatusBadRequest || h.tip(t) != before {
			t.Fatalf("a body without a login or a reason: %d %v", code, body)
		}
	}
	for _, path := range []string{
		"/securityOfficer/nowhere/7d0f3c2e", "/securityOfficer/demo/not%20a%20subject", "/admin/demo/7d0f3c2e", "/breakGlass/demo/7d0f3c2e",
		"/serviceAdmin/demo/7d0f3c2e",  // the platform tenant's people's alone
		"/tenantAuditor/demo/7d0f3c2e", // so is reading one tenant's evidence
		"/securityOfficer/demo/u-mia",  // not one of demo's administrators
	} {
		if code, body := h.do(t, "PUT", base+path, alice, `{"username":"x@demo.example","reason":"r"}`); code != http.StatusUnprocessableEntity || h.tip(t) != before {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}
	for name, b := range map[string]string{
		"an expiry that has passed": `{"username":"it@demo.example","reason":"r","expiresAt":"2020-01-01T00:00:00Z"}`,
		"another scope":             `{"username":"it@demo.example","reason":"r","scope":"tenant:demo"}`,
	} {
		if code, body := h.do(t, "PUT", base+"/securityOfficer/demo/7d0f3c2e", alice, b); code != http.StatusUnprocessableEntity || h.tip(t) != before {
			t.Fatalf("%s: %d %v", name, code, body)
		}
	}

	code, body = h.do(t, "PUT", base+"/securityOfficer/demo/7d0f3c2e", alice, `{"username":"it@demo.example","email":"it@demo.example","reason":"the deputy","expiresAt":"2099-01-01T00:00:00Z"}`)
	if code != http.StatusAccepted || body["commit"] != h.tip(t) {
		t.Fatalf("name: %d %v", code, body)
	}
	claim := dt.RemoteFile(t, h.remote, dt.ClaimPath)
	for _, line := range []string{"  platformAccess:", "    roleHolders:", "      - role: securityOfficer", "        scope: cluster", "        tenant: demo", "        subject: 7d0f3c2e",
		"        username: \"it@demo.example\"", "        email: \"it@demo.example\"", "        reason: the deputy", "        expiresAt: \"2099-01-01T00:00:00Z\""} {
		if !strings.Contains(claim+"\n", line+"\n") {
			t.Fatalf("the claim lacks %q:\n%s", line, claim)
		}
	}
	if code, body := h.do(t, "PUT", base+"/securityOfficer/demo/7d0f3c2e", alice, `{"username":"it@demo.example","email":"it@demo.example","reason":"the deputy","expiresAt":"2099-01-01T00:00:00Z"}`); code != http.StatusOK || body["status"] != "unchanged" {
		t.Fatalf("naming the same holder again: %d %v", code, body)
	}
	if code, _ := h.do(t, "PUT", base+"/auditor/demo/7d0f3c2e", alice, `{"username":"it@demo.example","reason":"r"}`); code != http.StatusAccepted {
		t.Fatalf("the same account, a second role: %d", code)
	}
	code, body = h.do(t, "PUT", base+"/perimeterApprover/demo/7d0f3c2e", alice, `{"username":"it@demo.example","reason":"runs the website"}`)
	if code != http.StatusAccepted || !strings.Contains(dt.RemoteFile(t, h.remote, dt.ClaimPath), "scope: \"tenant:demo\"") {
		t.Fatalf("a perimeter approver of their own tenant: %d %v", code, body)
	}

	code, body = h.do(t, "GET", base, alice, "")
	list, _ := body["roleHolders"].([]any)
	if code != http.StatusOK || len(list) != 3 {
		t.Fatalf("read back: %d %v", code, body)
	}
	first := list[0].(map[string]any)
	if first["role"] != "auditor" || first["tenant"] != "demo" || first["subject"] != "7d0f3c2e" || first["username"] != "it@demo.example" ||
		first["notInEffect"] != nil {
		t.Fatalf("first holder = %v", first)
	}
	// Whoever may audit the cluster may read who holds a role; a tenant's
	// administrator may not.
	if code, _ := h.do(t, "GET", base, h.token(t, "gentian", "audrey"), ""); code != http.StatusOK {
		t.Fatalf("the auditor reading the holders: %d", code)
	}
	if code, _ := h.do(t, "GET", base, tom, ""); code != http.StatusForbidden {
		t.Fatalf("a tenant's administrator reading the holders: %d", code)
	}

	if code, _ := h.do(t, "DELETE", base+"/securityOfficer/demo/7d0f3c2e", tom, ""); code != http.StatusForbidden {
		t.Fatalf("a tenant's administrator took the role away: %d", code)
	}
	if code, body := h.do(t, "DELETE", base+"/securityOfficer/demo/7d0f3c2e", alice, ""); code != http.StatusAccepted {
		t.Fatalf("remove: %d %v", code, body)
	}
	if code, body := h.do(t, "DELETE", base+"/securityOfficer/demo/7d0f3c2e", alice, ""); code != http.StatusOK || body["status"] != "unchanged" {
		t.Fatalf("removing what is gone: %d %v", code, body)
	}
	claim = dt.RemoteFile(t, h.remote, dt.ClaimPath)
	if strings.Contains(claim, "securityOfficer\n        tenant: demo") || !strings.Contains(claim, "- role: auditor") {
		t.Fatalf("after the removal:\n%s", claim)
	}
}

// Whether the platform's administrators may sign in to a tenant is the
// tenant's administrators' switch: read by whoever may view the tenant,
// written under can_administer, and a separate thing from the tenant being
// operated by the cluster.
func TestATenantOpensItselfToThePlatformsAdministrators(t *testing.T) {
	h, _ := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")
	mia := h.token(t, "tenant-demo", "mia")   // a member: may look
	tina := h.token(t, "tenant-solo", "tina") // another tenant's

	code, body := h.do(t, "GET", "/v1/tenants/demo/platform-access", mia, "")
	if code != http.StatusOK || body["adminsEnter"] != false {
		t.Fatalf("read: %d %v", code, body)
	}
	before := h.tip(t)
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/platform-access", mia, `{"adminsEnter":true}`); code != http.StatusForbidden || h.tip(t) != before {
		t.Fatalf("a member opened the tenant: %d", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/platform-access", tina, `{"adminsEnter":true}`); code != http.StatusForbidden || h.tip(t) != before {
		t.Fatalf("a stranger opened the tenant: %d", code)
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/platform-access", tom, `{}`); code != http.StatusBadRequest {
		t.Fatalf("a body without the switch: %d %v", code, body)
	}
	code, body = h.do(t, "PUT", "/v1/tenants/demo/platform-access", tom, `{"adminsEnter":true}`)
	if code != http.StatusAccepted || body["commit"] != h.tip(t) {
		t.Fatalf("open: %d %v", code, body)
	}
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(manifest, "  platformAccess:\n    adminsEnter: true") {
		t.Fatalf("the switch is not in the manifest:\n%s", manifest)
	}
	if _, body = h.do(t, "GET", "/v1/tenants/demo/platform-access", tom, ""); body["adminsEnter"] != true {
		t.Fatalf("read back: %v", body)
	}
	// The cluster's tenant list carries it, for the console's Enter action.
	_, body = h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/tenants", h.token(t, "gentian", "alice"), "")
	var demo map[string]any
	for _, raw := range body["tenants"].([]any) {
		if entry := raw.(map[string]any); entry["name"] == "demo" {
			demo = entry
		}
	}
	if demo == nil || demo["adminsEnter"] != true {
		t.Fatalf("the tenant list: %v", demo)
	}
	if code, body = h.do(t, "PUT", "/v1/tenants/demo/platform-access", tom, `{"adminsEnter":false}`); code != http.StatusAccepted {
		t.Fatalf("close: %d %v", code, body)
	}
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); strings.Contains(manifest, "platformAccess") {
		t.Fatalf("a closed switch is still in the manifest:\n%s", manifest)
	}
}

// An entry whose person is no longer an administrator of the tenant, or
// whose expiry has passed, stays in the claim and is listed as not in effect.
func TestARoleHolderNotInEffectIsListedAsSuch(t *testing.T) {
	h := startSeeded(t, nil, func(remote string) {
		claim := dt.RemoteFile(t, remote, dt.ClaimPath)
		claim = strings.TrimRight(claim, "\n") + "\n  platformAccess:\n    roleHolders:\n" +
			"      - role: auditor\n        scope: cluster\n        tenant: demo\n        subject: u-mia\n        username: mia@demo.example\n        reason: r\n" +
			"      - role: auditor\n        scope: cluster\n        tenant: demo\n        subject: 7d0f3c2e\n        username: it@demo.example\n        reason: r\n        expiresAt: \"2020-01-01T00:00:00Z\"\n" +
			"      - role: securityOfficer\n        scope: cluster\n        tenant: demo\n        subject: 7d0f3c2e\n        username: it@demo.example\n        reason: r\n"
		dt.Commit(t, remote, map[string]string{dt.ClaimPath: claim})
	})
	_, body := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/platform-roles/holders", h.token(t, "gentian", "alice"), "")
	got := map[string]any{}
	for _, raw := range body["roleHolders"].([]any) {
		e := raw.(map[string]any)
		got[e["role"].(string)+"/"+e["subject"].(string)] = e["notInEffect"]
	}
	want := map[string]any{
		"auditor/u-mia":            "no longer an administrator of demo",
		"auditor/7d0f3c2e":         "expired",
		"securityOfficer/7d0f3c2e": nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("not in effect: %v, want %v", got, want)
	}
}
