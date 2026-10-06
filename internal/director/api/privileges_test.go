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

const egressPath = "/v1/tenants/demo/privileges/nextcloud/egress/smtp-relay"
const waiverPath = "/v1/tenants/demo/privileges/nextcloud/podSecurity/run-as-root"

func reason(text string) string { return `{"reason":"` + text + `"}` }

// A tenant administrator answers for egress: the traffic leaves the tenant's
// own namespace, and can_approve_privilege is model v1's verb for it.
func TestTheTenantAdministratorApprovesEgress(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	code, body := h.do(t, "PUT", egressPath, tom, reason("agreed in the security review on the 14th"))
	if code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	code, body = h.do(t, "GET", "/v1/tenants/demo/privileges", tom, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %v", code, body)
	}
	list, _ := body["privileges"].([]any)
	if len(list) != 1 {
		t.Fatalf("privileges = %v", body["privileges"])
	}
	got, _ := list[0].(map[string]any)
	if got["privilege"] != "egress/smtp-relay" || got["install"] != "nextcloud" {
		t.Fatalf("grant = %v", got)
	}
	// The approver is the token's subject, never the body's.
	if got["approver"] != "tom" {
		t.Fatalf("approver = %v, want the caller", got["approver"])
	}
	// And the commit records the relation that actually authorised it, not
	// the can_view the route carries.
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main")
	if !strings.Contains(trailer, "can_approve_privilege tenant:demo") {
		t.Fatalf("the commit does not name the authority:\n%s", trailer)
	}
}

// A pod-security waiver weakens what protects the node, and every tenant on
// it. A tenant administrator's authority inside their own tenant is not that,
// however complete it is there.
func TestATenantAdministratorCannotWaivePodSecurity(t *testing.T) {
	h := start(t)
	before := h.tip(t)
	tom := h.token(t, "tenant-demo", "tom")

	code, body := h.do(t, "PUT", waiverPath, tom, reason("it would be convenient for us"))
	if code != http.StatusForbidden {
		t.Fatalf("PUT = %d %v, want 403", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "security officer") {
		t.Fatalf("refusal = %q; it should say whose approval this is", msg)
	}
	if h.tip(t) != before {
		t.Fatal("a refused grant committed anyway")
	}
	// And the same on the way out: revoking is the same authority as granting.
	if code, _ := h.do(t, "DELETE", waiverPath, tom, ""); code != http.StatusForbidden {
		t.Fatalf("DELETE = %d, want 403", code)
	}
}

// The security officer answers for it, and does not have to be a tenant
// administrator to do so -- which is the whole reason the check follows the
// kind rather than the route's relation.
func TestTheSecurityOfficerWaivesPodSecurity(t *testing.T) {
	h := start(t)
	sam := h.token(t, "gentian", "sam")

	code, body := h.do(t, "PUT", waiverPath, sam, reason("the converter forks as root; reviewed 2026-09-20"))
	if code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main")
	if !strings.Contains(trailer, "can_approve cluster:") {
		t.Fatalf("the commit does not name the security officer's verb:\n%s", trailer)
	}
	// Withdrawing it is the same authority, and puts the component back where
	// it was before the approval.
	if code, body := h.do(t, "DELETE", waiverPath, sam, ""); code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %v", code, body)
	}
}

// Configuring the cluster is not approving for it. The model's own fixture
// says alice has can_configure and can_approve: false, and this is what that
// distinction is worth.
func TestConfiguringTheClusterIsNotApprovingForIt(t *testing.T) {
	h := start(t)
	before := h.tip(t)
	alice := h.token(t, "gentian", "alice")

	if code, _ := h.do(t, "PUT", waiverPath, alice, reason("I run this cluster, after all")); code != http.StatusForbidden {
		t.Fatalf("PUT = %d, want 403", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused grant committed anyway")
	}
}

// A grant needs a reason in the approver's own words. The profile already said
// why it wants the privilege; this is the record of why somebody agreed, and
// it is the only part of the entry a caller supplies.
func TestAGrantNeedsAReason(t *testing.T) {
	h := start(t)
	before := h.tip(t)
	tom := h.token(t, "tenant-demo", "tom")

	for name, body := range map[string]string{
		"none":       `{}`,
		"too short":  reason("ok"),
		"whitespace": `{"reason":"           "}`,
		"not json":   `nonsense`,
	} {
		if code, _ := h.do(t, "PUT", egressPath, tom, body); code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400", name, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused grant committed anyway")
	}
}

// An expiry that has already passed grants nothing, so committing it would
// read as an approval while the component went on waiting.
func TestAnExpiryInThePastIsRefused(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	body := `{"reason":"agreed for one quarter only","expiresAt":"2020-01-01T00:00:00Z"}`
	if code, out := h.do(t, "PUT", egressPath, tom, body); code != http.StatusBadRequest {
		t.Fatalf("PUT = %d %v, want 400", code, out)
	}
	if code, _ := h.do(t, "PUT", egressPath, tom, `{"reason":"agreed for one quarter","expiresAt":"tomorrow"}`); code != http.StatusBadRequest {
		t.Fatalf("an unparseable expiry = %d, want 400", code)
	}
}

// A kind that is not one of the three is a 404: the route exists, the
// privilege named on it does not.
func TestAnUnknownPrivilegeKindIsNotFound(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	code, _ := h.do(t, "PUT", "/v1/tenants/demo/privileges/nextcloud/rootShell/everything",
		tom, reason("a kind somebody invented"))
	if code != http.StatusNotFound {
		t.Fatalf("PUT = %d, want 404", code)
	}
}

// Somebody who cannot see the tenant cannot approve for it either, which the
// route's own relation settles before any of the above runs.
func TestApprovingNeedsToSeeTheTenantAtAll(t *testing.T) {
	h := start(t)
	before := h.tip(t)
	tina := h.token(t, "tenant-solo", "tina")

	if code, _ := h.do(t, "PUT", egressPath, tina, reason("I would like this for my own tenant")); code != http.StatusForbidden {
		t.Fatalf("PUT = %d, want 403", code)
	}
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/privileges", tina, ""); code != http.StatusForbidden {
		t.Fatalf("GET = %d, want 403", code)
	}
	if code, _ := h.do(t, "PUT", egressPath, "", reason("no token at all here")); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT = %d, want 401", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused grant committed anyway")
	}
}
