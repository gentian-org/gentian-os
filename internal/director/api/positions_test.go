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
)

// A tenant's own position is declared by whoever manages its people, as a
// commit of one patch beside the tenant; read back, changed and removed.
func TestATenantsPositionIsDeclaredAsACommit(t *testing.T) {
	h := startWith(t, nil)
	tom := h.token(t, "tenant-demo", "tom") // administers demo
	base := "/v1/tenants/demo/positions"

	code, body := h.do(t, "GET", base, tom, "")
	if list, _ := body["positions"].([]any); code != http.StatusOK || len(list) != 0 {
		t.Fatalf("before: %d %v", code, body)
	}
	if shipped, _ := body["shipped"].([]any); len(shipped) != 3 {
		t.Errorf("the positions every tenant has = %v", body["shipped"])
	}

	manager := `{"title":"Sales manager","description":"Runs the sales team.","includes":["user"],
		"roles":[{"app":"odoo-sales-ce","role":"manager"},{"app":"nextcloud","role":"user"},{"app":"nextcloud","role":"user"}]}`
	if code, body := h.do(t, "PUT", base+"/sales-manager", tom, manager); code != http.StatusAccepted {
		t.Fatalf("declared: %d %v", code, body)
	}
	file := h.remoteFile(t, "demo", "positions.yaml")
	for _, want := range []string{"kind: Tenant", "name: demo", "- description: Runs the sales team.", "name: sales-manager",
		"title: Sales manager", "role: manager", "app: odoo-sales-ce"} {
		if !strings.Contains(file, want) {
			t.Fatalf("positions.yaml lacks %q:\n%s", want, file)
		}
	}
	if strings.Count(file, "app: nextcloud") != 1 {
		t.Errorf("a role named twice is written twice:\n%s", file)
	}
	// Declared again as it is, nothing is committed.
	before := h.tip(t)
	if code, body := h.do(t, "PUT", base+"/sales-manager", tom, manager); code != http.StatusOK || body["status"] != "unchanged" || h.tip(t) != before {
		t.Errorf("declared again: %d %v", code, body)
	}
	// A second position that includes the first; both are read back.
	if code, body := h.do(t, "PUT", base+"/executive", tom, `{"includes":["sales-manager"]}`); code != http.StatusAccepted {
		t.Fatalf("a second position: %d %v", code, body)
	}
	code, body = h.do(t, "GET", base, tom, "")
	if list, _ := body["positions"].([]any); code != http.StatusOK || len(list) != 2 {
		t.Fatalf("read back: %d %v", code, body)
	}

	// Refused, each for what it is, and nothing committed.
	before = h.tip(t)
	for name, c := range map[string]struct {
		method, path, body string
		want               int
	}{
		"a position every tenant has":       {"PUT", base + "/user", `{}`, http.StatusUnprocessableEntity},
		"a name that is none":               {"PUT", base + "/Sales_Manager", `{}`, http.StatusUnprocessableEntity},
		"including what the tenant has not": {"PUT", base + "/deputy", `{"includes":["ghost"]}`, http.StatusUnprocessableEntity},
		"a circle":                          {"PUT", base + "/sales-manager", `{"includes":["executive"]}`, http.StatusUnprocessableEntity},
		"a tenant role that is none":        {"PUT", base + "/deputy", `{"roles":[{"role":"manager"}]}`, http.StatusUnprocessableEntity},
		"an app that reaches elsewhere":     {"PUT", base + "/deputy", `{"roles":[{"app":"../other","role":"user"}]}`, http.StatusUnprocessableEntity},
		"a body that is none":               {"PUT", base + "/deputy", `not json`, http.StatusBadRequest},
		"removing one another includes":     {"DELETE", base + "/sales-manager", ``, http.StatusUnprocessableEntity},
		"removing one the tenant has not":   {"DELETE", base + "/ghost", ``, http.StatusNotFound},
	} {
		if code, body := h.do(t, c.method, c.path, tom, c.body); code != c.want {
			t.Errorf("%s: %d %v, want %d", name, code, body, c.want)
		}
	}
	if h.tip(t) != before {
		t.Error("a refused declaration was committed")
	}

	// Only whoever manages the tenant's people: not a user of it, not
	// another tenant's administrator.
	for who, token := range map[string]string{"a user": h.token(t, "tenant-demo", "mia"), "another tenant's administrator": h.token(t, "tenant-solo", "tina")} {
		if code, _ := h.do(t, "PUT", base+"/deputy", token, `{}`); code != http.StatusForbidden {
			t.Errorf("%s declaring a position: %d, want 403", who, code)
		}
		if code, _ := h.do(t, "GET", base, token, ""); code != http.StatusForbidden {
			t.Errorf("%s reading positions: %d, want 403", who, code)
		}
	}
	if h.tip(t) != before {
		t.Error("a refused caller committed")
	}

	// Removed in the order that holds together; the last one takes the
	// patch with it.
	for _, name := range []string{"executive", "sales-manager"} {
		if code, body := h.do(t, "DELETE", base+"/"+name, tom, ""); code != http.StatusAccepted {
			t.Fatalf("remove %s: %d %v", name, code, body)
		}
	}
	if left := h.remoteFile(t, "demo", "positions.yaml"); left != "" {
		t.Errorf("the patch is left with nothing to declare:\n%s", left)
	}
}
