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

// A role holder is an account of a tenant's realm the claim names, written
// by the cluster's administrator alone, read by whoever may audit, and taken
// away by the same commit path.
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
	if roles, _ := body["roles"].([]any); len(roles) != 2 || roles[0] != "auditor" || roles[1] != "securityOfficer" {
		t.Fatalf("the roles a holder may hold: %v", body["roles"])
	}

	before := h.tip(t)
	if code, _ := h.do(t, "PUT", base+"/securityOfficer/demo/7d0f3c2e", tom, `{"username":"tom@demo.example"}`); code != http.StatusForbidden || h.tip(t) != before {
		t.Fatalf("a tenant's administrator named a holder: %d", code)
	}
	if code, body := h.do(t, "PUT", base+"/securityOfficer/demo/7d0f3c2e", alice, `{}`); code != http.StatusBadRequest || h.tip(t) != before {
		t.Fatalf("a body without a login: %d %v", code, body)
	}
	for _, path := range []string{"/securityOfficer/nowhere/7d0f3c2e", "/securityOfficer/demo/not%20a%20subject", "/admin/demo/7d0f3c2e", "/breakGlass/demo/7d0f3c2e"} {
		if code, body := h.do(t, "PUT", base+path, alice, `{"username":"x@demo.example"}`); code != http.StatusUnprocessableEntity || h.tip(t) != before {
			t.Fatalf("%s: %d %v", path, code, body)
		}
	}

	code, body = h.do(t, "PUT", base+"/securityOfficer/demo/7d0f3c2e", alice, `{"username":"it@demo.example","email":"it@demo.example"}`)
	if code != http.StatusAccepted || body["commit"] != h.tip(t) {
		t.Fatalf("name: %d %v", code, body)
	}
	claim := dt.RemoteFile(t, h.remote, dt.ClaimPath)
	for _, line := range []string{"  platformAccess:", "    roleHolders:", "      - role: securityOfficer", "        tenant: demo", "        subject: 7d0f3c2e", "        username: \"it@demo.example\"", "        email: \"it@demo.example\""} {
		if !strings.Contains(claim+"\n", line+"\n") {
			t.Fatalf("the claim lacks %q:\n%s", line, claim)
		}
	}
	if code, body := h.do(t, "PUT", base+"/securityOfficer/demo/7d0f3c2e", alice, `{"username":"it@demo.example","email":"it@demo.example"}`); code != http.StatusOK || body["status"] != "unchanged" {
		t.Fatalf("naming the same holder again: %d %v", code, body)
	}
	if code, _ := h.do(t, "PUT", base+"/auditor/demo/7d0f3c2e", alice, `{"username":"it@demo.example"}`); code != http.StatusAccepted {
		t.Fatalf("the same account, a second role: %d", code)
	}

	code, body = h.do(t, "GET", base, alice, "")
	list, _ := body["roleHolders"].([]any)
	if code != http.StatusOK || len(list) != 2 {
		t.Fatalf("read back: %d %v", code, body)
	}
	first := list[0].(map[string]any)
	if first["role"] != "auditor" || first["tenant"] != "demo" || first["subject"] != "7d0f3c2e" || first["username"] != "it@demo.example" {
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
