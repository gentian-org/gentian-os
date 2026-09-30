/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api_test

import (
	"net/http"
	"testing"
)

// What became of a tenant's apps is the operator's answer, relayed to whoever
// may see the tenant. A member sees it too: whether the wiki is up is not an
// administrator's secret.
func TestWhatBecameOfATenantsAppsIsRelayed(t *testing.T) {
	h, _ := startWithOperator(t)

	for _, who := range []string{"tom", "mia"} {
		code, body := h.do(t, "GET", "/v1/tenants/demo/apps/status", h.token(t, "tenant-demo", who), "")
		if code != http.StatusOK {
			t.Fatalf("%s: %d %v", who, code, body)
		}
		apps, _ := body["apps"].([]any)
		if len(apps) != 2 {
			t.Fatalf("%s: apps = %v", who, body["apps"])
		}
		failing, _ := apps[1].(map[string]any)
		if failing["phase"] != "failing" || failing["failure"] == "" {
			t.Fatalf("the failing app lost its reason on the way: %v", failing)
		}
	}

	// Another tenant's administrator holds nothing here.
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/apps/status", h.token(t, "tenant-solo", "tina"), ""); code != http.StatusForbidden {
		t.Fatalf("a stranger read another tenant's apps: %d", code)
	}
}

// Purging and provisioning are actions: nothing is committed, the person is
// named on the request, and the profile is all that travels.
func TestPurgingAndProvisioningAreActionsByANamedPerson(t *testing.T) {
	h, op := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)

	for _, action := range []string{"purge-app", "provision-app"} {
		// A body that says more than which profile is refused outright: the
		// tenant is the path's to name, and nothing else is the caller's.
		if code, _ := h.do(t, "POST", "/v1/tenants/demo/actions/"+action, tom,
			`{"profile":"xwiki-ce","tenant":"somebody-else"}`); code != http.StatusBadRequest {
			t.Fatalf("%s took a body naming another tenant: %d", action, code)
		}
		code, body := h.do(t, "POST", "/v1/tenants/demo/actions/"+action, tom, `{"profile":"xwiki-ce"}`)
		if code != http.StatusAccepted {
			t.Fatalf("%s: %d %v", action, code, body)
		}
		if op.lastAction != action || op.actor != "tom@example.com" {
			t.Fatalf("%s arrived as %q by %q", action, op.lastAction, op.actor)
		}
		if len(op.lastBody) != 1 || op.lastBody["profile"] != "xwiki-ce" {
			t.Fatalf("%s carried more than the profile: %v", action, op.lastBody)
		}
	}
	if h.tip(t) != before {
		t.Fatal("an action wrote to git")
	}
}

func TestOnlyWhoMayInstallPurgesAndOnlyWhoMayGrantProvisions(t *testing.T) {
	h, op := startWithOperator(t)

	mia := h.token(t, "tenant-demo", "mia")   // a member
	tina := h.token(t, "tenant-solo", "tina") // administers another tenant
	for _, action := range []string{"purge-app", "provision-app"} {
		for who, token := range map[string]string{"a member": mia, "a stranger": tina} {
			if code, _ := h.do(t, "POST", "/v1/tenants/demo/actions/"+action, token, `{"profile":"xwiki-ce"}`); code != http.StatusForbidden {
				t.Fatalf("%s ran %s: %d", who, action, code)
			}
		}
	}
	if op.lastAction != "" {
		t.Fatalf("a refused action reached the operator: %q", op.lastAction)
	}
}

// The profile lands in a request to the operator, so it is a name or nothing.
func TestAnActionNamesAProfile(t *testing.T) {
	h, op := startWithOperator(t)
	tom := h.token(t, "tenant-demo", "tom")

	for _, body := range []string{``, `{}`, `{"profile":""}`, `{"profile":"../demo"}`, `{"profile":"Not A Name"}`} {
		if code, _ := h.do(t, "POST", "/v1/tenants/demo/actions/purge-app", tom, body); code != http.StatusBadRequest {
			t.Fatalf("body %q: %d", body, code)
		}
	}
	if op.lastAction != "" {
		t.Fatalf("an unnamed purge reached the operator: %q", op.lastAction)
	}
}
