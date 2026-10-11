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

// entryOf is one entry of the read, by app instance and entry name.
func entryOf(t *testing.T, body map[string]any, install, name string) map[string]any {
	t.Helper()
	entries, ok := body["entries"].([]any)
	if !ok {
		t.Fatalf("the read has no entries: %v", body)
	}
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		if entry["install"] == install && entry["exposureName"] == name {
			return entry
		}
	}
	t.Fatalf("no entry %s/%s in %v", install, name, entries)
	return nil
}

func exposuresPath(tenant string) string { return "/v1/tenants/" + tenant + "/exposures" }

// registryFile is a tenant's exposures.yaml with the entries given, each
// "install name reviewAt [expiresAt]".
func registryFile(tenant string, entries ...string) (path, body string) {
	body = "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: " + tenant + "\nspec:\n  exposures:\n"
	for _, e := range entries {
		f := strings.Fields(e)
		body += "    - install: " + f[0] + "\n      exposureName: " + f[1] + "\n      owner: olga\n      reviewAt: " + f[2] + "\n"
		if len(f) > 3 {
			body += "      expiresAt: " + f[3] + "\n"
		}
	}
	return strings.TrimSuffix(dt.TenantPath(tenant), "tenant.yaml") + "exposures.yaml", body
}

func stamp(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }

// Before anything is approved the read says what each installed app asks to
// have on the internet: the address, the paths, and that nobody signs in.
// Approving it changes its state and nothing else about it.
func TestTheReadShowsWhatAnAppAsksToPublishBeforeItIsApproved(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")

	code, body := h.do(t, "GET", exposuresPath("demo"), tom, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %v", code, body)
	}
	// The registry is as it was for whoever reads it today.
	for _, key := range []string{"live", "expired", "reviewDue"} {
		if list, ok := body[key].([]any); !ok || len(list) != 0 {
			t.Fatalf("%s = %v, want an empty list", key, body[key])
		}
	}
	if n := len(body["entries"].([]any)); n != 1 {
		t.Fatalf("entries = %v; the app's page behind sign-in is not something to approve", body["entries"])
	}
	asked := entryOf(t, body, "nextcloud", "shares")
	if asked["state"] != "requested" || asked["approval"] != nil {
		t.Fatalf("an entry nobody approved = %v", asked)
	}
	if asked["host"] != "share-nextcloud.demo."+dt.KernelDomain {
		t.Fatalf("host = %v", asked["host"])
	}
	if paths, _ := asked["paths"].([]any); len(paths) != 2 || paths[0] != "/public.php/" || paths[1] != "/s/" {
		t.Fatalf("paths = %v", asked["paths"])
	}
	if deny, _ := asked["denyPaths"].([]any); len(deny) != 1 || deny[0] != "/s/admin/" {
		t.Fatalf("denyPaths = %v", asked["denyPaths"])
	}
	if asked["anyoneWithoutSignIn"] != true || asked["authMode"] != "none" ||
		!strings.Contains(asked["access"].(string), "anyone on the internet without sign-in") {
		t.Fatalf("who can reach it = %v", asked)
	}
	if asked["mainAddress"] != false || asked["mainAddressRule"] != nil {
		t.Fatalf("an entry under the tenant's own address was marked for the main address: %v", asked)
	}

	if code, body := h.do(t, "PUT", sharesPath, tom, `{"reason":"shared calendars"}`); code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	_, body = h.do(t, "GET", exposuresPath("demo"), tom, "")
	approved := entryOf(t, body, "nextcloud", "shares")
	approval, _ := approved["approval"].(map[string]any)
	if approved["state"] != "approved" || approval["owner"] != "tom" || approval["reason"] != "shared calendars" ||
		approval["reviewAt"] == nil || approval["publishedAt"] == nil {
		t.Fatalf("the approved entry = %v", approved)
	}
	if approved["host"] != asked["host"] {
		t.Fatalf("approving moved the address: %v, was %v", approved["host"], asked["host"])
	}
}

// The read is can_view, as it was: whoever may see the tenant, and nobody
// else.
func TestTheRequestsAreReadByWhoeverMaySeeTheTenant(t *testing.T) {
	h := startTenancy(t, &residueOperator{}, "single", nil)
	if code, body := h.do(t, "GET", exposuresPath("user"), h.token(t, "tenant-user", "ulf"), ""); code != http.StatusOK {
		t.Fatalf("a member of the tenant: %d %v", code, body)
	}
	for who, token := range map[string]string{
		"another tenant's administrator": h.token(t, "tenant-demo", "tom"),
		"somebody with no relation":      h.token(t, "tenant-user", "nobody"),
	} {
		if code, _ := h.do(t, "GET", exposuresPath("user"), token, ""); code != http.StatusForbidden {
			t.Errorf("%s read what tenant user asks to publish: %d", who, code)
		}
	}
}

// The address is the one the operator publishes at: under the tenant's own
// domain, the cluster's for the user tenant of a single-tenancy cluster, and
// a bound domain when the repository binds one.
func TestTheAddressFollowsTheTenantsDomain(t *testing.T) {
	multi := start(t)
	tom := multi.token(t, "tenant-demo", "tom")
	dt.Commit(t, multi.remote, map[string]string{
		strings.TrimSuffix(dt.TenantPath("demo"), "tenant.yaml") + "domain.yaml": "apiVersion: gentianos.io/v1alpha1\nkind: TenantDomain\nmetadata:\n  name: demo\nspec:\n  domain: acme.example\n",
		dt.ClaimPath: claimWith(),
	})
	// A read is as fresh as the checkout; a write refreshes it.
	if code, body := multi.do(t, "PUT", sharesPath, tom, `{}`); code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	_, body := multi.do(t, "GET", exposuresPath("demo"), tom, "")
	if got := entryOf(t, body, "nextcloud", "shares")["host"]; got != "share-nextcloud.acme.example" {
		t.Fatalf("on a bound domain: host = %v", got)
	}

	single := startTenancy(t, &residueOperator{}, "single", websites())
	uma := single.token(t, "tenant-user", "uma")
	_, body = single.do(t, "GET", exposuresPath("user"), uma, "")
	if got := entryOf(t, body, "nextcloud", "shares")["host"]; got != "share-nextcloud."+dt.KernelDomain {
		t.Fatalf("the user tenant of a single-tenancy cluster: host = %v", got)
	}
	// A website for the main address: the bare domain, and the rule the
	// approver has to acknowledge comes with it.
	site := entryOf(t, body, "website", "site")
	rule, _ := site["mainAddressRule"].(string)
	if site["state"] != "requested" || site["host"] != dt.KernelDomain || site["mainAddress"] != true ||
		!strings.Contains(rule, "no third-party scripts") || strings.Contains(rule, "acknowledgeMainAddressRule") {
		t.Fatalf("a website for the main address = %v", site)
	}
	// One surface holds it at a time: once one is approved the other has no
	// address, and says who holds it.
	if code, body := single.do(t, "PUT", userSitePath, uma, mainAddress); code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	_, body = single.do(t, "GET", exposuresPath("user"), uma, "")
	if held := entryOf(t, body, "website", "site"); held["state"] != "approved" || held["host"] != dt.KernelDomain {
		t.Fatalf("the website that holds the main address = %v", held)
	}
	other := entryOf(t, body, "blog", "site")
	if note, _ := other["note"].(string); other["host"] != nil || !strings.Contains(note, "already held by website/site") {
		t.Fatalf("a second website = %v; it has no address while another holds it", other)
	}

	// On a multi-tenancy cluster no tenant has the main address.
	many := startTenancy(t, &residueOperator{}, "multi", websites())
	_, body = many.do(t, "GET", exposuresPath("user"), many.token(t, "tenant-user", "uma"), "")
	nowhere := entryOf(t, body, "website", "site")
	if note, _ := nowhere["note"].(string); nowhere["host"] != nil || !strings.Contains(note, "tenancy mode is multi") {
		t.Fatalf("a website for the main address on a multi-tenancy cluster = %v", nowhere)
	}
	if got := entryOf(t, body, "nextcloud", "shares")["host"]; got != "share-nextcloud.user."+dt.KernelDomain {
		t.Fatalf("a tenant of a multi-tenancy cluster: host = %v", got)
	}
}

// An approved entry is approved, due for review, or expired, and each is said.
func TestAnApprovedEntrySaysWhetherItIsDueOrExpired(t *testing.T) {
	cases := map[string]string{
		"approved":  "nextcloud shares " + stamp(48*time.Hour),
		"reviewDue": "nextcloud shares " + stamp(-48*time.Hour),
		"expired":   "nextcloud shares " + stamp(-72*time.Hour) + " " + stamp(-24*time.Hour),
	}
	for want, entry := range cases {
		path, file := registryFile("demo", entry)
		h := startSeeded(t, nil, func(remote string) { dt.Commit(t, remote, map[string]string{path: file}) })
		_, body := h.do(t, "GET", exposuresPath("demo"), h.token(t, "tenant-demo", "tom"), "")
		got := entryOf(t, body, "nextcloud", "shares")
		approval, _ := got["approval"].(map[string]any)
		if got["state"] != want || approval["owner"] != "olga" {
			t.Errorf("want %s: %v", want, got)
		}
	}
}

// An approval names an app the tenant has installed and an entry its profile
// declares for the internet, or it is refused and nothing is committed.
func TestAnApprovalOfWhatDoesNotExistIsRefused(t *testing.T) {
	h := start(t)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	for what, tc := range map[string]struct{ path, says string }{
		"an app the tenant does not have":   {"/v1/tenants/demo/exposures/ghost/api", "has no app instance named ghost installed"},
		"an app only the cluster has":       {"/v1/tenants/demo/exposures/wiki/shares", "has no app instance named wiki installed"},
		"an entry the profile lacks":        {"/v1/tenants/demo/exposures/nextcloud/caldav", "declares no entry named caldav for the internet, and none of that name that asks for anything else an approver decides. What it declares: shares"},
		"the app's page behind sign-in":     {"/v1/tenants/demo/exposures/nextcloud/web", "is not one for the internet"},
		"a platform component, never asked": {"/v1/tenants/demo/exposures/desktop/front", "is a component the platform itself ships"},
	} {
		code, body := h.do(t, "PUT", tc.path, tom, `{"reason":"a public page"}`)
		msg, _ := body["error"].(string)
		if code != http.StatusUnprocessableEntity || !strings.Contains(msg, tc.says) || !strings.Contains(msg, "Nothing was changed") {
			t.Errorf("%s: %d %v, want 422 saying %q", what, code, body, tc.says)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused approval committed anyway")
	}
	// Refused before the main address is asked about: there is nothing to
	// acknowledge a rule for.
	code, body := h.do(t, "PUT", "/v1/tenants/demo/exposures/ghost/api", tom, mainAddress)
	if msg, _ := body["error"].(string); code != http.StatusUnprocessableEntity || !strings.Contains(msg, "ghost") {
		t.Fatalf("an app that does not exist, for the main address: %d %v", code, body)
	}
}

// What was recorded before the check is left as it is, shown as matching
// nothing, and can still be withdrawn.
func TestARegistryEntryThatMatchesNothingIsShownAndCanBeWithdrawn(t *testing.T) {
	path, file := registryFile("demo",
		"ghost api "+stamp(48*time.Hour),
		"nextcloud caldav "+stamp(48*time.Hour),
		"nextcloud web "+stamp(48*time.Hour),
		"nextcloud shares "+stamp(48*time.Hour))
	h := startSeeded(t, nil, func(remote string) { dt.Commit(t, remote, map[string]string{path: file}) })
	tom := h.token(t, "tenant-demo", "tom")

	_, body := h.do(t, "GET", exposuresPath("demo"), tom, "")
	if live, _ := body["live"].([]any); len(live) != 4 {
		t.Fatalf("the registry itself changed: live = %v", body["live"])
	}
	for name, says := range map[string]string{
		"ghost api":        "has no app instance named ghost installed",
		"nextcloud caldav": "declares no entry named caldav",
		"nextcloud web":    "is not one for the internet",
	} {
		f := strings.Fields(name)
		got := entryOf(t, body, f[0], f[1])
		note, _ := got["note"].(string)
		approval, _ := got["approval"].(map[string]any)
		if got["state"] != "unmatched" || !strings.Contains(note, says) || !strings.Contains(note, "publishes nothing") ||
			got["host"] != nil || approval["owner"] != "olga" {
			t.Errorf("%s = %v, want it marked as matching nothing: %q", name, got, says)
		}
	}
	if got := entryOf(t, body, "nextcloud", "shares"); got["state"] != "approved" {
		t.Fatalf("the entry that does match = %v", got)
	}

	if code, body := h.do(t, "DELETE", "/v1/tenants/demo/exposures/ghost/api", tom, ""); code != http.StatusAccepted {
		t.Fatalf("withdrawing an entry that matches nothing: %d %v", code, body)
	}
	_, body = h.do(t, "GET", exposuresPath("demo"), tom, "")
	if n := len(body["entries"].([]any)); n != 3 {
		t.Fatalf("after the withdrawal: entries = %v", body["entries"])
	}
	// It cannot be renewed: a review is an approval, and is checked as one.
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/exposures/nextcloud/caldav", tom, `{}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("reviewing an entry that matches nothing: %d", code)
	}
}

// The platform's own page is published by the installer from a profile the
// repository does not hold. It is not reported as matching nothing, and it
// can be reviewed; nothing new of such a component can be published.
func TestThePlatformsOwnPageCanBeReviewed(t *testing.T) {
	path, file := registryFile("demo", "concierge front "+stamp(-48*time.Hour))
	h := startSeeded(t, nil, func(remote string) { dt.Commit(t, remote, map[string]string{path: file}) })
	tom := h.token(t, "tenant-demo", "tom")

	_, body := h.do(t, "GET", exposuresPath("demo"), tom, "")
	page := entryOf(t, body, "concierge", "front")
	if note, _ := page["note"].(string); page["state"] != "reviewDue" || !strings.Contains(note, "the platform itself ships") {
		t.Fatalf("the platform's page = %v", page)
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/exposures/concierge/front", tom, `{"reason":"still the sign-in page"}`); code != http.StatusAccepted {
		t.Fatalf("reviewing it: %d %v", code, body)
	}
	_, body = h.do(t, "GET", exposuresPath("demo"), tom, "")
	if page := entryOf(t, body, "concierge", "front"); page["state"] != "approved" {
		t.Fatalf("after the review = %v", page)
	}
	// Whether it is for the main address is the profile's to say, and that
	// cannot be read: a review does not change what the registry holds.
	before := h.tip(t)
	code, body := h.do(t, "PUT", "/v1/tenants/demo/exposures/concierge/front", tom, mainAddress)
	if msg, _ := body["error"].(string); code != http.StatusUnprocessableEntity ||
		!strings.Contains(msg, "A review keeps the setting the entry has") || !strings.Contains(msg, "Nothing was changed") {
		t.Fatalf("a review that changes the main-address setting: %d %v", code, body)
	}
	if h.tip(t) != before {
		t.Fatal("the refused review committed anyway")
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/exposures/concierge/other", tom, `{}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("a new entry of a platform component: %d %v", code, body)
	}
}

// An add-on switched on inside an app is installed too, and what it declares
// for the internet is listed with the app's.
func TestAnAddonsEntriesAreListed(t *testing.T) {
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.CataloguePath("deck.yaml"): dt.PublishingProfileYAML("deck", "boards", false),
			dt.TenantPath("demo"):         dt.TenantYAML("demo") + "    addons: [deck]\n",
		})
	})
	_, body := h.do(t, "GET", exposuresPath("demo"), h.token(t, "tenant-demo", "tom"), "")
	if got := entryOf(t, body, "deck", "boards"); got["state"] != "requested" || got["host"] != "share-deck.demo."+dt.KernelDomain {
		t.Fatalf("the add-on's entry = %v", got)
	}
}

// The main address takes the profile's author and the approver both saying
// so. An approval that says it of an entry the profile does not declare for
// the main address, or does not say it of one the profile does, is something
// the operator publishes nothing for: it is refused, and nothing is committed.
func TestAnApprovalWhoseMainAddressSettingIsNotTheEntrysIsRefused(t *testing.T) {
	h := startTenancy(t, &residueOperator{}, "single", websites())
	uma := h.token(t, "tenant-user", "uma")
	before := h.tip(t)

	for what, tc := range map[string]struct{ path, body, says string }{
		"the main address for an entry under the tenant's own address": {
			"/v1/tenants/user/exposures/nextcloud/shares", mainAddress,
			"the profile of nextcloud does not declare entry shares for the main address"},
		"the same without the acknowledgement": {
			"/v1/tenants/user/exposures/nextcloud/shares", unacknowledged,
			"the profile of nextcloud does not declare entry shares for the main address"},
		"a website for the main address, approved as an ordinary entry": {
			userSitePath, `{"reason":"our public website"}`,
			"the profile of website declares entry site for the cluster's main address"},
		"the same with apex false": {
			userSitePath, `{"apex":false,"acknowledgeMainAddressRule":true}`,
			"the profile of website declares entry site for the cluster's main address"},
	} {
		code, body := h.do(t, "PUT", tc.path, uma, tc.body)
		msg, _ := body["error"].(string)
		if code != http.StatusUnprocessableEntity || !strings.Contains(msg, tc.says) ||
			!strings.Contains(msg, "would publish nothing") || !strings.HasSuffix(msg, ". Nothing was changed") {
			t.Errorf("%s: %d %v, want 422 saying %q", what, code, body, tc.says)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused approval committed anyway")
	}
	_, body := h.do(t, "GET", exposuresPath("user"), uma, "")
	for _, name := range []string{"nextcloud shares", "website site"} {
		f := strings.Fields(name)
		if got := entryOf(t, body, f[0], f[1]); got["state"] != "requested" || got["approval"] != nil {
			t.Errorf("%s after the refusals = %v", name, got)
		}
	}

	// It is asked of whoever may publish, and of nobody else: a member is
	// told they may not, not what the entry is.
	if code, _ := h.do(t, "PUT", userSitePath, h.token(t, "tenant-user", "ulf"), `{}`); code != http.StatusForbidden {
		t.Fatalf("a member, with a setting that does not match: %d", code)
	}
	// With the setting the entry has, each is approved as before.
	if code, body := h.do(t, "PUT", userSitePath, uma, mainAddress); code != http.StatusAccepted {
		t.Fatalf("the website, for the main address: %d %v", code, body)
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/user/exposures/nextcloud/shares", uma, `{}`); code != http.StatusAccepted {
		t.Fatalf("the entry under the tenant's own address: %d %v", code, body)
	}
}

// What was recorded with the wrong setting before the check stays, is shown
// as it was -- with no address and the reason -- and can be withdrawn. It
// cannot be renewed on that setting.
func TestARegistryEntryWithTheWrongMainAddressSettingStaysAndIsNotRenewed(t *testing.T) {
	// shares was approved for the main address and is not declared for it;
	// site is declared for it and was approved as an ordinary entry.
	path, file := registryFile("user", "nextcloud shares "+stamp(48*time.Hour), "website site "+stamp(48*time.Hour))
	file = strings.Replace(file, "      owner: olga\n", "      owner: olga\n      apex: true\n", 1)
	files := websites()
	files[path] = file
	h := startTenancy(t, &residueOperator{}, "single", files)
	uma := h.token(t, "tenant-user", "uma")

	_, body := h.do(t, "GET", exposuresPath("user"), uma, "")
	if live, _ := body["live"].([]any); len(live) != 2 {
		t.Fatalf("the registry itself changed: live = %v", body["live"])
	}
	for name, says := range map[string]string{
		"nextcloud shares": "the profile does not declare it for that address",
		"website site":     "is published only when the approver says so",
	} {
		f := strings.Fields(name)
		got := entryOf(t, body, f[0], f[1])
		note, _ := got["note"].(string)
		if got["state"] != "approved" || got["host"] != nil || !strings.Contains(note, says) {
			t.Errorf("%s = %v, want it approved, without an address, saying %q", name, got, says)
		}
	}

	before := h.tip(t)
	// Renewed as recorded, each is the mismatch it was.
	for path, payload := range map[string]string{
		"/v1/tenants/user/exposures/nextcloud/shares": mainAddress,
		userSitePath: `{"reason":"still our website"}`,
	} {
		if code, body := h.do(t, "PUT", path, uma, payload); code != http.StatusUnprocessableEntity {
			t.Errorf("renewing %s on the setting it has: %d %v", path, code, body)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused renewal committed anyway")
	}
	// Corrected, it is approved, and has an address again.
	if code, body := h.do(t, "PUT", "/v1/tenants/user/exposures/nextcloud/shares", uma, `{}`); code != http.StatusAccepted {
		t.Fatalf("the corrected approval: %d %v", code, body)
	}
	_, body = h.do(t, "GET", exposuresPath("user"), uma, "")
	if got := entryOf(t, body, "nextcloud", "shares"); got["host"] != "share-nextcloud."+dt.KernelDomain {
		t.Fatalf("after the correction = %v", got)
	}
	// And the other is withdrawn the way every entry is.
	if code, body := h.do(t, "DELETE", userSitePath, uma, ""); code != http.StatusAccepted {
		t.Fatalf("withdrawing the mismatched entry: %d %v", code, body)
	}
}
