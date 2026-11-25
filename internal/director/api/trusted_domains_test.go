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

const trustedDomains = "/v1/tenants/demo/trusted-domains"

// A tenant keeps trusted domains only once the cluster's administrator
// delegated the list, and then its perimeter approver adds and removes them:
// not a member, and not before.
func TestThePerimeterApproverKeepsTheTenantsTrustedDomainsOnceDelegated(t *testing.T) {
	h := start(t)
	alice, tom, mia := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-demo", "mia")
	delegation := clusterTenant("demo", "/trusted-domains-delegation")

	if code, body := h.do(t, "PUT", trustedDomains+"/example.org", tom, ""); code != http.StatusForbidden {
		t.Fatalf("a domain was trusted before the list was delegated: %d %v", code, body)
	}
	if code, _ := h.do(t, "PUT", delegation, tom, ""); code != http.StatusForbidden {
		t.Fatalf("the tenant delegated the list to itself: %d", code)
	}
	if code, body := h.do(t, "PUT", delegation, alice, ""); code != http.StatusAccepted {
		t.Fatalf("delegating: %d %v", code, body)
	}
	if code, body := h.do(t, "PUT", delegation, alice, ""); code != http.StatusOK || body["status"] != "unchanged" {
		t.Fatalf("delegating again: %d %v", code, body)
	}

	if code, _ := h.do(t, "PUT", trustedDomains+"/example.org", mia, ""); code != http.StatusForbidden {
		t.Fatalf("a member trusted a domain: %d", code)
	}
	code, body := h.do(t, "PUT", trustedDomains+"/example.org", tom, "")
	if code != http.StatusAccepted || body["domain"] != "example.org" {
		t.Fatalf("PUT = %d %v", code, body)
	}
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main")
	if !strings.Contains(trailer, "trust the domain example.org") || !strings.Contains(trailer, "can_approve_privilege tenant:demo") {
		t.Fatalf("the commit does not say what was done and by what authority:\n%s", trailer)
	}
	if code, body := h.do(t, "PUT", trustedDomains+"/example.org", tom, ""); code != http.StatusOK || body["status"] != "unchanged" {
		t.Fatalf("trusting it again: %d %v", code, body)
	}

	code, body = h.do(t, "GET", trustedDomains, mia, "")
	if code != http.StatusOK || body["delegated"] != true {
		t.Fatalf("GET = %d %v", code, body)
	}
	if list, _ := body["domains"].([]any); len(list) != 1 || list[0] != "example.org" {
		t.Fatalf("domains = %v", body["domains"])
	}

	if code, _ := h.do(t, "DELETE", trustedDomains+"/example.org", mia, ""); code != http.StatusForbidden {
		t.Fatalf("a member removed a domain: %d", code)
	}
	if code, body := h.do(t, "DELETE", trustedDomains+"/other.example", tom, ""); code != http.StatusNotFound {
		t.Fatalf("removing what is not listed: %d %v", code, body)
	}
	if code, body := h.do(t, "DELETE", trustedDomains+"/example.org", tom, ""); code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %v", code, body)
	}
	if _, body := h.do(t, "GET", trustedDomains, tom, ""); len(body["domains"].([]any)) != 0 {
		t.Fatalf("domains after removing = %v", body["domains"])
	}
}

// What is not a domain is refused before anything is written: a bare word,
// an address, upper case, a name with a port or a wildcard.
func TestWhatIsNoDomainIsNotTrusted(t *testing.T) {
	h := start(t)
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	if code, body := h.do(t, "PUT", clusterTenant("demo", "/trusted-domains-delegation"), alice, ""); code != http.StatusAccepted {
		t.Fatalf("delegating: %d %v", code, body)
	}
	for _, name := range []string{"com", "10.0.0.1", "Example.org", "example.org:443", "*.example.org", "example..org"} {
		if code, body := h.do(t, "PUT", trustedDomains+"/"+name, tom, ""); code != http.StatusUnprocessableEntity {
			t.Errorf("%q: %d %v", name, code, body)
		}
	}
	if _, body := h.do(t, "GET", trustedDomains, tom, ""); len(body["domains"].([]any)) != 0 {
		t.Fatalf("domains = %v", body["domains"])
	}
}

// Taking the delegation back removes nothing, and the list can no longer be
// changed by the tenant.
func TestWithoutTheDelegationTheListStaysAndIsNotTheTenantsToChange(t *testing.T) {
	h := start(t)
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	delegation := clusterTenant("demo", "/trusted-domains-delegation")
	h.do(t, "PUT", delegation, alice, "")
	h.do(t, "PUT", trustedDomains+"/example.org", tom, "")
	if code, body := h.do(t, "DELETE", delegation, alice, ""); code != http.StatusAccepted {
		t.Fatalf("taking the delegation back: %d %v", code, body)
	}
	code, body := h.do(t, "GET", trustedDomains, tom, "")
	if list, _ := body["domains"].([]any); code != http.StatusOK || body["delegated"] != false || len(list) != 1 {
		t.Fatalf("GET = %d %v", code, body)
	}
	if code, _ := h.do(t, "DELETE", trustedDomains+"/example.org", tom, ""); code != http.StatusForbidden {
		t.Fatalf("the tenant changed a list that is not delegated: %d", code)
	}
}
