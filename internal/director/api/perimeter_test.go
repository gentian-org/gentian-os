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

// Who approves a public address, and who says who does.
//
// The fixture's tenants are the three cases there are. demo is operated by
// its cluster and its administrators were let approve; solo administers
// itself and nobody switched anything on; the model's own tests hold the
// fourth, a tenant that is operated and where nothing was switched on.

const soloShares = "/v1/tenants/solo/exposures/nextcloud/shares"

func TestAPublicAddressIsApprovedByWhoeverHoldsTheRelationAndNobodyElse(t *testing.T) {
	for _, c := range []struct {
		who, realm, subject, path string
		want                      int
	}{
		// demo: operated, administrators approve.
		{"the cluster's administrator, in a tenant the cluster operates", "gentian", "alice", sharesPath, http.StatusAccepted},
		{"the tenant's administrator, where administrators approve", "tenant-demo", "tom", sharesPath, http.StatusAccepted},
		{"a member of the perimeter group", "tenant-demo", "pat", sharesPath, http.StatusAccepted},
		{"a member of the tenant", "tenant-demo", "mia", sharesPath, http.StatusForbidden},
		{"another tenant's administrator", "tenant-solo", "tina", sharesPath, http.StatusForbidden},
		// solo: administers itself, nothing switched on.
		{"the cluster's administrator, in a tenant that withdrew the operator", "gentian", "alice", soloShares, http.StatusForbidden},
		{"the tenant's administrator, where nothing was switched on", "tenant-solo", "tina", soloShares, http.StatusForbidden},
		{"a member of that tenant's perimeter group", "tenant-solo", "paul", soloShares, http.StatusAccepted},
		{"another tenant's approver", "tenant-demo", "pat", soloShares, http.StatusForbidden},
	} {
		h := start(t)
		before := h.tip(t)
		code, body := h.do(t, "PUT", c.path, h.token(t, c.realm, c.subject), publishBody(time.Now().Add(24*time.Hour)))
		if code != c.want {
			t.Errorf("%s: %d %v, want %d", c.who, code, body, c.want)
		}
		if moved := h.tip(t) != before; moved != (c.want == http.StatusAccepted) {
			t.Errorf("%s: answered %d, and the repository moved = %v", c.who, code, moved)
		}
		// Withdrawing is the same relation.
		code, _ = h.do(t, "DELETE", c.path, h.token(t, c.realm, c.subject), "")
		if refused := code == http.StatusForbidden; refused != (c.want == http.StatusForbidden) {
			t.Errorf("%s: withdrawing answered %d", c.who, code)
		}
	}
}

// The switch is the cluster administrator's alone: nobody who holds a
// relation on the tenant reaches it, the tenant's administrator least of all,
// and there is no such route under a tenant.
func TestWhetherATenantsAdministratorsApproveIsTheClusterAdministratorsToSay(t *testing.T) {
	h := start(t)
	before := h.tip(t)
	for who, token := range map[string]string{
		"the tenant's administrator": h.token(t, "tenant-demo", "tom"),
		"a perimeter approver":       h.token(t, "tenant-demo", "pat"),
		"a member":                   h.token(t, "tenant-demo", "mia"),
		"solo's administrator":       h.token(t, "tenant-solo", "tina"),
		"an auditor":                 h.token(t, "gentian", "audrey"),
		"the security officer":       h.token(t, "gentian", "sam"),
	} {
		for _, tenant := range []string{"demo", "solo"} {
			for _, method := range []string{"PUT", "DELETE"} {
				if code, out := h.do(t, method, clusterTenant(tenant, "/perimeter-delegation"), token, ""); code != http.StatusForbidden {
					t.Errorf("%s, %s on %s: %d %v, want 403", who, method, tenant, code, out)
				}
			}
		}
		// Nor by creating a tenant that starts with it.
		if code, out := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants", token,
			`{"name":"mine","perimeter":{"adminsApprove":true},"catalogue":{"delegated":true}}`); code != http.StatusForbidden {
			t.Errorf("%s created a tenant: %d %v", who, code, out)
		}
	}
	tom := h.token(t, "tenant-demo", "tom")
	for _, method := range []string{"PUT", "DELETE", "POST", "PATCH"} {
		if code, _ := h.do(t, method, "/v1/tenants/demo/perimeter-delegation", tom, `{"adminsApprove":true}`); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/tenants/demo/perimeter-delegation answered %d", method, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
}

// The cluster's administrator switches it on and off, and each is one commit
// to the tenant's manifest: git holds who may, and the operator projects it.
func TestTheSwitchIsACommitToTheTenantsManifest(t *testing.T) {
	h := start(t)
	alice := h.token(t, "gentian", "alice")
	listed := func() map[string]string {
		t.Helper()
		code, out := h.do(t, "GET", "/v1/clusters/"+dt.Cluster+"/tenants", alice, "")
		if code != http.StatusOK {
			t.Fatalf("list: %d %v", code, out)
		}
		got := map[string]string{}
		for _, entry := range out["tenants"].([]any) {
			e := entry.(map[string]any)
			got[e["name"].(string)] = strings.Join([]string{boolWord(e["adminsApprove"]), boolWord(e["cataloguesDelegated"])}, " ")
		}
		return got
	}
	if got := listed(); got["demo"] != "false false" || got["solo"] != "false false" {
		t.Fatalf("before anything was switched on: %v", got)
	}

	// solo administers itself; the switch is still the cluster's.
	code, out := h.do(t, "PUT", clusterTenant("solo", "/perimeter-delegation"), alice, "")
	if code != http.StatusAccepted || out["status"] != "updated" || out["adminsApprove"] != true || out["commit"] == nil {
		t.Fatalf("switching on: %d %v", code, out)
	}
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("solo"))
	if !strings.Contains(manifest, "\n  perimeter:\n    adminsApprove: true") {
		t.Fatalf("the manifest does not say so:\n%s", manifest)
	}
	message := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main")
	if !strings.Contains(message, "can_configure cluster:"+dt.Cluster) || !strings.Contains(message, "approve public addresses") {
		t.Fatalf("the commit does not say who decided what:\n%s", message)
	}
	if got := listed(); got["solo"] != "true false" || got["demo"] != "false false" {
		t.Fatalf("after switching solo on: %v", got)
	}
	tip := h.tip(t)
	if code, out := h.do(t, "PUT", clusterTenant("solo", "/perimeter-delegation"), alice, ""); code != http.StatusOK || out["status"] != "unchanged" || h.tip(t) != tip {
		t.Fatalf("the same again: %d %v", code, out)
	}

	// Beside the other switch, each leaves the other alone.
	if code, _ := h.do(t, "PUT", clusterTenant("solo", "/catalogue-delegation"), alice, ""); code != http.StatusAccepted {
		t.Fatalf("delegating catalogues: %d", code)
	}
	if got := listed(); got["solo"] != "true true" {
		t.Fatalf("with both on: %v", got)
	}

	code, out = h.do(t, "DELETE", clusterTenant("solo", "/perimeter-delegation"), alice, "")
	if code != http.StatusAccepted || out["adminsApprove"] != false {
		t.Fatalf("switching off: %d %v", code, out)
	}
	manifest = dt.RemoteFile(t, h.remote, dt.TenantPath("solo"))
	if strings.Contains(manifest, "perimeter") || strings.Contains(manifest, "adminsApprove") || !strings.Contains(manifest, "delegated: true") {
		t.Fatalf("switched off, the manifest still says so, or lost the other switch:\n%s", manifest)
	}
	if got := listed(); got["solo"] != "false true" {
		t.Fatalf("after switching off: %v", got)
	}
	if code, out := h.do(t, "DELETE", clusterTenant("solo", "/perimeter-delegation"), alice, ""); code != http.StatusOK || out["status"] != "unchanged" {
		t.Fatalf("switching off what is off: %d %v", code, out)
	}

	// A tenant that is not there.
	if code, _ := h.do(t, "PUT", clusterTenant("nobody", "/perimeter-delegation"), alice, ""); code != http.StatusNotFound {
		t.Fatalf("a tenant that does not exist: %d", code)
	}
	// The platform tenant's administrators are the cluster's.
	adoptKernelRealm(t, h, "demo")
	tip = h.tip(t)
	if code, out := h.do(t, "PUT", clusterTenant("demo", "/perimeter-delegation"), alice, ""); code != http.StatusConflict || h.tip(t) != tip {
		t.Fatalf("the platform tenant: %d %v", code, out)
	}
}

// A tenant is created with the two switches as its creator states them, and
// with neither when nothing is said.
func TestATenantIsCreatedWithTheSwitchesItsCreatorStates(t *testing.T) {
	h := start(t)
	alice := h.token(t, "gentian", "alice")
	tenants := "/v1/clusters/" + dt.Cluster + "/tenants"
	for name, c := range map[string]struct {
		body                string
		approve, catalogues bool
	}{
		"quiet":   {`{"name":"quiet"}`, false, false},
		"said-no": {`{"name":"said-no","perimeter":{"adminsApprove":false},"catalogue":{"delegated":false}}`, false, false},
		"approve": {`{"name":"approve","perimeter":{"adminsApprove":true}}`, true, false},
		"sources": {`{"name":"sources","catalogue":{"delegated":true}}`, false, true},
		"both":    {`{"name":"both","displayName":"Both","perimeter":{"adminsApprove":true},"catalogue":{"delegated":true}}`, true, true},
	} {
		if code, out := h.do(t, "POST", tenants, alice, c.body); code != http.StatusAccepted {
			t.Fatalf("%s: %d %v", name, code, out)
		}
		manifest := dt.RemoteFile(t, h.remote, dt.TenantPath(name)) + "\n"
		if got := strings.Contains(manifest, "\n  perimeter:\n    adminsApprove: true\n"); got != c.approve {
			t.Errorf("%s: administrators approve = %v, want %v\n%s", name, got, c.approve, manifest)
		}
		if got := strings.Contains(manifest, "\n  catalogue:\n    delegated: true\n"); got != c.catalogues {
			t.Errorf("%s: catalogues delegated = %v, want %v\n%s", name, got, c.catalogues, manifest)
		}
		if !c.approve && strings.Contains(manifest, "perimeter") || !c.catalogues && strings.Contains(manifest, "catalogue:") {
			t.Errorf("%s: a switch nobody set is written:\n%s", name, manifest)
		}
	}
	// A body that names a switch the route does not have is refused whole.
	if code, _ := h.do(t, "POST", tenants, alice, `{"name":"typo","perimeter":{"adminsAprove":true}}`); code != http.StatusBadRequest {
		t.Fatalf("a misspelt switch: %d", code)
	}
}

func boolWord(v any) string {
	if b, ok := v.(bool); ok && b {
		return "true"
	}
	return "false"
}
