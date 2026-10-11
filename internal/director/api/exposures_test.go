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

// Seeing a tenant is not publishing from it. mia is a member and sam is the
// cluster's security officer, who sees every tenant; neither holds can_expose.
func TestPublishingNeedsTheExposeRelation(t *testing.T) {
	h := start(t)
	before := h.tip(t)

	for name, tok := range map[string]string{
		"a member":           h.token(t, "tenant-demo", "mia"),
		"a security officer": h.token(t, "gentian", "sam"),
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

const (
	userSitePath = "/v1/tenants/user/exposures/website/site"
	mainAddress  = `{"apex":true,"acknowledgeMainAddressRule":true,"reason":"our public website"}`
	// unacknowledged is the same request from an approver who has not been
	// told the rule for the main address yet.
	unacknowledged = `{"apex":true,"reason":"our public website"}`
)

// websites is a repository in which tenants user and demo have two apps
// installed beside Nextcloud, each declaring a website for the cluster's
// main address.
func websites() map[string]string {
	files := map[string]string{
		dt.CataloguePath("website.yaml"): dt.PublishingProfileYAML("website", "site", true),
		dt.CataloguePath("blog.yaml"):    dt.PublishingProfileYAML("blog", "site", true),
	}
	for _, tenant := range []string{"user", "demo"} {
		files[dt.TenantPath(tenant)] = dt.TenantYAML(tenant) + "  - profile: website\n  - profile: blog\n"
	}
	return files
}

// On a single-tenancy cluster the user tenant's perimeter approver may ask
// for the cluster's main address, with the same relation and the same record
// as any other surface, and the registry says which entry it is.
func TestTheUserTenantPublishesAWebsiteOnTheMainAddress(t *testing.T) {
	h := startTenancy(t, &residueOperator{}, "single", websites())
	uma := h.token(t, "tenant-user", "uma")

	// A member of the tenant may not: it is can_expose, like every surface.
	if code, _ := h.do(t, "PUT", userSitePath, h.token(t, "tenant-user", "ulf"), mainAddress); code != http.StatusForbidden {
		t.Fatalf("a member published on the main address: %d", code)
	}
	code, body := h.do(t, "PUT", userSitePath, uma, mainAddress)
	if code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	message := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main")
	if !strings.Contains(message, "on the cluster's main address") || !strings.Contains(message, "can_expose tenant:user") {
		t.Fatalf("the commit does not say what was published, or by which relation:\n%s", message)
	}
	patch := dt.Git(t, "", "--git-dir", h.remote, "show", "main:"+strings.TrimSuffix(dt.TenantPath("user"), "tenant.yaml")+"exposures.yaml")
	if !strings.Contains(patch, "apex: true") {
		t.Fatalf("the Tenant patch does not carry apex:\n%s", patch)
	}

	_, body = h.do(t, "GET", "/v1/tenants/user/exposures", uma, "")
	live, _ := body["live"].([]any)
	if len(live) != 1 {
		t.Fatalf("live = %v", body["live"])
	}
	entry, _ := live[0].(map[string]any)
	if entry["apex"] != true || entry["owner"] != "uma" || entry["reviewAt"] == "" || entry["publishedAt"] == "" {
		t.Fatalf("the registry entry = %v; it must say apex, the owner and the dates", entry)
	}
	if entry["apexAcknowledgedBy"] != "uma" || entry["apexAcknowledgedAt"] == nil || entry["apexAcknowledgedAt"] == "" {
		t.Fatalf("the registry entry = %v; it must say who acknowledged the rule for the main address, and when", entry)
	}
	if !strings.Contains(patch, "apexAcknowledgedBy: uma") || !strings.Contains(patch, "apexAcknowledgedAt: ") {
		t.Fatalf("the Tenant patch does not record the acknowledgement:\n%s", patch)
	}

	// One surface at a time: a second is refused, and told who holds it.
	before := h.tip(t)
	code, body = h.do(t, "PUT", "/v1/tenants/user/exposures/blog/site", uma, mainAddress)
	msg, _ := body["error"].(string)
	if code != http.StatusConflict || !strings.Contains(msg, "website/site") || !strings.Contains(msg, "uma") {
		t.Fatalf("a second website on the main address: %d %v, want 409 naming the holder", code, body)
	}
	if h.tip(t) != before {
		t.Fatal("the refused request committed anyway")
	}
	// The same surface again is a review, not a second one.
	// (200 when the review lands in the second the entry was written in.)
	if code, body := h.do(t, "PUT", userSitePath, uma, mainAddress); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("reviewing the website: %d %v", code, body)
	}
	// A surface that is not for the main address is not in its way.
	if code, body := h.do(t, "PUT", "/v1/tenants/user/exposures/nextcloud/shares", uma, `{"reason":"shared calendars"}`); code != http.StatusAccepted {
		t.Fatalf("another surface beside the website: %d %v", code, body)
	}

	// Withdrawn the way every surface is; the address is then free again.
	if code, _ := h.do(t, "DELETE", userSitePath, uma, ""); code != http.StatusAccepted {
		t.Fatalf("DELETE = %d", code)
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/user/exposures/blog/site", uma, mainAddress); code != http.StatusAccepted {
		t.Fatalf("after the withdrawal, another website: %d %v", code, body)
	}
}

// Anywhere but the user tenant of a single-tenancy cluster the main address
// is not a tenant's to ask for, and nothing is committed.
func TestTheMainAddressIsRefusedEverywhereElse(t *testing.T) {
	cases := []struct {
		what, mode, path, realm, who string
		platform                     bool
	}{
		{"a multi-tenancy cluster", "multi", userSitePath, "tenant-user", "uma", false},
		{"a cluster that declares no mode", "", userSitePath, "tenant-user", "uma", false},
		{"a tenant that is not the user tenant", "single", "/v1/tenants/demo/exposures/website/site", "tenant-demo", "tom", false},
		{"the platform tenant", "single", "/v1/tenants/demo/exposures/website/site", "tenant-demo", "tom", true},
	}
	for _, tc := range cases {
		h := startTenancy(t, &residueOperator{}, tc.mode, websites())
		if tc.platform {
			adoptKernelRealm(t, h, "demo", "website", "blog")
		}
		before := h.tip(t)
		tok := h.token(t, tc.realm, tc.who)
		code, body := h.do(t, "PUT", tc.path, tok, mainAddress)
		msg, _ := body["error"].(string)
		if code != http.StatusConflict || !strings.Contains(msg, "single-tenancy") {
			t.Errorf("%s: %d %v, want 409 with the reason", tc.what, code, body)
		}
		if h.tip(t) != before {
			t.Errorf("%s: the refused request committed anyway", tc.what)
		}
		// The same entry without apex is no way round it: the profile
		// declares it for the main address, and the operator would publish
		// nothing for an approval that does not say so. The platform
		// tenant's zone is the one the operator does not ask that of.
		code, body = h.do(t, "PUT", tc.path, tok, `{"reason":"a public page"}`)
		if want := map[bool]int{false: http.StatusUnprocessableEntity, true: http.StatusAccepted}[tc.platform]; code != want {
			t.Errorf("%s: the website without apex: %d %v, want %d", tc.what, code, body, want)
		}
		// An entry that is not for the main address is an ordinary surface,
		// as before.
		ordinary := strings.TrimSuffix(tc.path, "website/site") + "nextcloud/shares"
		if code, body := h.do(t, "PUT", ordinary, tok, `{"reason":"shared calendars"}`); code != http.StatusAccepted {
			t.Errorf("%s: an ordinary surface: %d %v", tc.what, code, body)
		}
	}
}

// A website on the main address is published only by an approver who was told
// what a script there can do and said the site is one whose scripts the
// organisation controls. Without that word the answer is the warning, naming
// the field, and nothing is committed -- on a first publication and on a
// review alike.
func TestTheMainAddressNeedsTheApproversAcknowledgement(t *testing.T) {
	h := startTenancy(t, &residueOperator{}, "single", websites())
	uma := h.token(t, "tenant-user", "uma")

	refused := func(when, tok, payload string) {
		t.Helper()
		before := h.tip(t)
		code, body := h.do(t, "PUT", userSitePath, tok, payload)
		msg, _ := body["error"].(string)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d %v, want 400", when, code, body)
		}
		for _, want := range []string{
			`"acknowledgeMainAddressRule": true`, "cookies", "signing in",
			"an account its author chose", "no third-party scripts", "no pages uploaded by users",
			"Nothing was changed",
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("%s: the refusal does not say %q:\n%s", when, want, msg)
			}
		}
		if h.tip(t) != before {
			t.Fatalf("%s: the refused request committed anyway", when)
		}
	}
	refused("no acknowledgement", uma, unacknowledged)
	refused("an acknowledgement of false", uma, `{"apex":true,"acknowledgeMainAddressRule":false}`)

	if code, body := h.do(t, "PUT", userSitePath, uma, mainAddress); code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	// A review is asked again: the entry's old acknowledgement is not the
	// reviewer's.
	refused("a review without one", uma, unacknowledged)

	// An ordinary surface is not asked, and records none even when sent one.
	if code, body := h.do(t, "PUT", "/v1/tenants/user/exposures/nextcloud/shares", uma,
		`{"acknowledgeMainAddressRule":true,"reason":"shared calendars"}`); code != http.StatusAccepted {
		t.Fatalf("an ordinary surface: %d %v", code, body)
	}
	_, body := h.do(t, "GET", "/v1/tenants/user/exposures", uma, "")
	live, _ := body["live"].([]any)
	for _, l := range live {
		entry, _ := l.(map[string]any)
		_, has := entry["apexAcknowledgedBy"]
		if apex := entry["apex"] == true; apex != has {
			t.Fatalf("entry %v: an acknowledgement belongs to the main address's entry and to no other", entry)
		}
	}
}
