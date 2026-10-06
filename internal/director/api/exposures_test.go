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
	"time"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

const sharesPath = "/v1/tenants/demo/exposures/nextcloud/shares"

func publishBody(expires time.Time) string {
	return fmt.Sprintf(`{"expiresAt":%q,"reason":"the team shares calendars with the client"}`,
		expires.UTC().Format(time.RFC3339))
}

// Publishing is its own decision, asked as its own relation, and recorded as
// its own commit.
func TestAPerimeterApproverPublishesASurface(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	code, body := h.do(t, "PUT", sharesPath, tom, publishBody(time.Now().Add(30*24*time.Hour)))
	if code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	// The commit names can_expose, not admin: putting something on the
	// internet is never something that merely happened while administering.
	trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main")
	if !strings.Contains(trailer, "can_expose tenant:demo") {
		t.Fatalf("the commit does not name can_expose:\n%s", trailer)
	}

	code, body = h.do(t, "GET", "/v1/tenants/demo/exposures", tom, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %v", code, body)
	}
	live, _ := body["live"].([]any)
	if len(live) != 1 {
		t.Fatalf("live = %v", body["live"])
	}
	got, _ := live[0].(map[string]any)
	if got["install"] != "nextcloud" || got["exposureName"] != "shares" {
		t.Fatalf("entry = %v", got)
	}
	// The owner is the token's subject, never the body's.
	if got["owner"] != "tom" {
		t.Fatalf("owner = %v, want the caller", got["owner"])
	}
}

// What is public is looked at again, whether or not it ends. A surface gets a
// review date for saying nothing and may stay without an expiry -- a tenant's
// website is not taken down the day nobody renewed it -- but nobody may put
// the review off past a year, and an overdue review is reported, not acted on.
func TestAPublishedSurfaceIsAlwaysReviewedAndEndsOnlyIfAsked(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	// Saying nothing: a review date, and no expiry.
	if code, _ := h.do(t, "PUT", sharesPath, tom, `{"reason":"shared calendars"}`); code != http.StatusAccepted {
		t.Fatalf("PUT with nothing said = %d", code)
	}
	code, body := h.do(t, "GET", "/v1/tenants/demo/exposures", tom, "")
	live, _ := body["live"].([]any)
	entry, _ := live[0].(map[string]any)
	if entry["reviewAt"] == nil || entry["reviewAt"] == "" {
		t.Fatalf("no review date was recorded: %v (code %d)", entry, code)
	}
	if _, has := entry["expiresAt"]; has {
		t.Fatalf("an expiry nobody asked for: %v", entry)
	}
	if due, _ := body["reviewDue"].([]any); len(due) != 0 {
		t.Fatalf("a review not yet due is reported: %v", due)
	}

	// A review put off ten years is refused rather than quietly clamped.
	late := `{"reason":"x","reviewAt":"` + time.Now().Add(10*365*24*time.Hour).UTC().Format(time.RFC3339) + `"}`
	if code, out := h.do(t, "PUT", sharesPath, tom, late); code != http.StatusBadRequest {
		t.Fatalf("a review in ten years = %d %v, want 400", code, out)
	}
	// An expiry already over publishes nothing.
	past := publishBody(time.Now().Add(-time.Hour))
	if code, _ := h.do(t, "PUT", sharesPath, tom, past); code != http.StatusBadRequest {
		t.Fatalf("an expiry in the past = %d, want 400", code)
	}
	// One that ends before its review is reviewed by ending.
	soon := time.Now().Add(24 * time.Hour)
	if code, _ := h.do(t, "PUT", sharesPath, tom, publishBody(soon)); code != http.StatusAccepted {
		t.Fatalf("a day-long exposure = %d", code)
	}
	_, body = h.do(t, "GET", "/v1/tenants/demo/exposures", tom, "")
	entry, _ = body["live"].([]any)[0].(map[string]any)
	if entry["expiresAt"] != entry["reviewAt"] || entry["expiresAt"] == nil {
		t.Fatalf("expiry and review of a short exposure: %v", entry)
	}
}

// Withdrawing takes it down, and the URL stops answering once the operator
// has seen the commit.
func TestWithdrawingRemovesItFromTheRegistry(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	if code, _ := h.do(t, "PUT", sharesPath, tom, publishBody(time.Now().Add(24*time.Hour))); code != http.StatusAccepted {
		t.Fatal("could not publish")
	}
	if code, body := h.do(t, "DELETE", sharesPath, tom, ""); code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %v", code, body)
	}
	_, body := h.do(t, "GET", "/v1/tenants/demo/exposures", tom, "")
	if live, _ := body["live"].([]any); len(live) != 0 {
		t.Fatalf("still published: %v", live)
	}
	// Withdrawing what is not published changes nothing and says so.
	if code, _ := h.do(t, "DELETE", sharesPath, tom, ""); code != http.StatusOK {
		t.Fatalf("a second withdrawal = %d, want 200 unchanged", code)
	}
}

// Administering a tenant is not publishing from it. alice runs the cluster
// and mia is a member; neither holds can_expose.
func TestPublishingNeedsTheExposeRelation(t *testing.T) {
	h := start(t)
	before := h.tip(t)

	for name, tok := range map[string]string{
		"a member":             h.token(t, "tenant-demo", "mia"),
		"a cluster configurer": h.token(t, "gentian", "alice"),
	} {
		code, _ := h.do(t, "PUT", sharesPath, tok, publishBody(time.Now().Add(24*time.Hour)))
		if code != http.StatusForbidden {
			t.Errorf("%s published a surface: %d", name, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused publish committed anyway")
	}
}

// A perimeter approver who is not an administrator may publish. pat holds
// can_expose and nothing else, which is the model's own fixture for the role
// existing separately from running the tenant.
func TestAnApproverNeedNotBeAnAdministrator(t *testing.T) {
	h := start(t)
	pat := h.token(t, "tenant-demo", "pat")

	if code, body := h.do(t, "PUT", sharesPath, pat, publishBody(time.Now().Add(24*time.Hour))); code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	// Reading the registry is can_view, which pat does not hold: publishing
	// and seeing the tenant are different questions, and pat was given one.
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/exposures", pat, ""); code != http.StatusForbidden {
		t.Fatalf("GET = %d; reading the tenant's registry is can_view", code)
	}
}
