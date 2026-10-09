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

// kindsProfile is an app that asks for all three things an approver decides:
// a public address for anyone, a public address that passes the caller's
// credential to the app, and, behind sign-in, its own Authorization header.
// extra goes on the entry behind sign-in.
func kindsProfile(name, davMode, extra string) string {
	return dt.ProfileYAML(name) +
		"  expose:\n" +
		"    - name: web\n      surface: gateway\n      authMode: oidc\n" + extra +
		"      backend:\n        service: " + name + "\n        port: 8080\n" +
		"    - name: shares\n      surface: perimeter\n      authMode: none\n      subDomain: share-" + name + "\n      paths: [\"/s/\"]\n" +
		"      backend:\n        service: " + name + "\n        port: 8080\n" +
		"    - name: dav\n      surface: perimeter\n      authMode: " + davMode + "\n      subDomain: dav-" + name + "\n      paths: [\"/remote.php/dav/\"]\n" +
		"      backend:\n        service: " + name + "\n        port: 8080\n"
}

func startKinds(t *testing.T, davMode, webExtra string, more map[string]string) *harness {
	t.Helper()
	return startSeeded(t, nil, func(remote string) {
		files := map[string]string{dt.CataloguePath("nextcloud.yaml"): kindsProfile("nextcloud", davMode, webExtra)}
		for k, v := range more {
			files[k] = v
		}
		dt.Commit(t, remote, files)
	})
}

const keepsHeader = "      clientAuthorization: app\n"

func registryOf(t *testing.T, h *harness, tenant string) string {
	t.Helper()
	return dt.Git(t, "", "--git-dir", h.remote, "show",
		"main:"+strings.TrimSuffix(dt.TenantPath(tenant), "tenant.yaml")+"exposures.yaml")
}

// Every entry says which kind it is and, in the director's own words, what
// approving it allows. The words are what the console and the command line
// show, so each sentence the approver has to have read is held here.
func TestEveryRequestSaysItsKindAndWhatApprovingItAllows(t *testing.T) {
	h := startKinds(t, "app", keepsHeader, nil)
	tom := h.token(t, "tenant-demo", "tom")
	code, body := h.do(t, "GET", exposuresPath("demo"), tom, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %v", code, body)
	}
	if n := len(body["entries"].([]any)); n != 3 {
		t.Fatalf("entries = %v, want the three that ask for something", body["entries"])
	}
	kinds, _ := body["kinds"].(map[string]any)
	for _, k := range []string{"public", "publicAppCredential", "signInAppAuthorization"} {
		if s, _ := kinds[k].(string); s == "" {
			t.Fatalf("kinds = %v: no words for %s", body["kinds"], k)
		}
	}

	public := entryOf(t, body, "nextcloud", "shares")
	if public["kind"] != "public" || public["kindLabel"] != "Public address" || public["publicAddress"] != true ||
		public["passesCredential"] != false || public["anyoneWithoutSignIn"] != true {
		t.Fatalf("a public address for anyone = %v", public)
	}
	for _, says := range []string{"anyone on the internet without sign-in", "No credential is passed to the app"} {
		if !strings.Contains(public["access"].(string), says) {
			t.Errorf("a public address does not say %q: %v", says, public["access"])
		}
	}
	if !strings.Contains(public["rateLimit"].(string), "20 requests a second") || !strings.Contains(public["rateLimit"].(string), "429") {
		t.Errorf("the limit of a public address = %v", public["rateLimit"])
	}

	passes := entryOf(t, body, "nextcloud", "dav")
	if passes["kind"] != "publicAppCredential" || passes["publicAddress"] != true || passes["passesCredential"] != true ||
		passes["anyoneWithoutSignIn"] != false || passes["authMode"] != "app" || passes["state"] != "requested" {
		t.Fatalf("a public address that passes the credential = %v", passes)
	}
	if passes["kindLabel"] != "Public address that passes the caller's credential to the app" {
		t.Fatalf("kindLabel = %v", passes["kindLabel"])
	}
	if passes["host"] != "dav-nextcloud.demo."+dt.KernelDomain {
		t.Fatalf("host = %v", passes["host"])
	}
	for _, says := range []string{
		"The caller's credential (the Authorization header) is passed to the app as the caller sent it, and the app alone checks it.",
		"The platform does not know or check who calls.",
		"An app password or token of a person who was removed from the tenant keeps working until the app itself revokes it.",
		"Cookies are not passed to the app",
	} {
		if !strings.Contains(passes["access"].(string), says) {
			t.Errorf("the approver is not told %q: %v", says, passes["access"])
		}
	}
	limit, _ := passes["rateLimit"].(string)
	if !strings.Contains(limit, "5 requests a second") || !strings.Contains(limit, "429") || !strings.Contains(limit, "guess at a password") {
		t.Errorf("the limit of an entry that passes a credential = %v", limit)
	}

	kept := entryOf(t, body, "nextcloud", "web")
	if kept["kind"] != "signInAppAuthorization" || kept["publicAddress"] != false || kept["passesCredential"] != true ||
		kept["anyoneWithoutSignIn"] != false || kept["mainAddress"] != false || kept["rateLimit"] != nil {
		t.Fatalf("an entry behind sign-in that keeps its header = %v", kept)
	}
	if kept["kindLabel"] != "Behind sign-in: keeps the app's own Authorization header" {
		t.Fatalf("kindLabel = %v", kept["kindLabel"])
	}
	// Its address is the one it already has behind sign-in, and the whole of it.
	if kept["host"] != "nextcloud.demo."+dt.KernelDomain {
		t.Fatalf("host = %v", kept["host"])
	}
	if paths, _ := kept["paths"].([]any); len(paths) != 1 || paths[0] != "/" {
		t.Fatalf("paths = %v", kept["paths"])
	}
	for _, says := range []string{
		"This is not a public address.",
		"People still have to sign in, and still have to be allowed to use the app",
		"puts no token of its own there",
		"Until approved, the header is removed before the app",
	} {
		if !strings.Contains(kept["access"].(string), says) {
			t.Errorf("the approver is not told %q: %v", says, kept["access"])
		}
	}
}

// An approval records the kind the entry declares, in the registry and in
// the commit, whatever the request's body says; and the registry's lists
// carry it, so a reader can tell what is a public address and what is not.
func TestAnApprovalRecordsTheKindTheEntryDeclares(t *testing.T) {
	h := startKinds(t, "app", keepsHeader, nil)
	tom := h.token(t, "tenant-demo", "tom")
	for entry, kind := range map[string]string{"shares": "public", "dav": "publicAppCredential", "web": "signInAppAuthorization"} {
		path := "/v1/tenants/demo/exposures/nextcloud/" + entry
		if code, body := h.do(t, "PUT", path, tom, `{"reason":"asked for","kind":"`+kind+`"}`); code != http.StatusAccepted {
			t.Fatalf("approve %s as %s = %d %v", entry, kind, code, body)
		}
		message := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%s", "main")
		switch kind {
		case "publicAppCredential":
			if !strings.Contains(message, "passes the caller's credential to the app") {
				t.Errorf("the commit does not say a credential is passed on: %q", message)
			}
		case "signInAppAuthorization":
			if !strings.HasPrefix(message, "Approve ") || !strings.Contains(message, "not a public address") {
				t.Errorf("the commit reads as a publication: %q", message)
			}
		}
	}
	file := registryOf(t, h, "demo")
	for _, want := range []string{
		"exposureName: dav\n      kind: publicAppCredential\n      owner: tom",
		"exposureName: shares\n      kind: public\n      owner: tom",
		"exposureName: web\n      kind: signInAppAuthorization\n      owner: tom",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("the registry lacks %q:\n%s", want, file)
		}
	}
	_, body := h.do(t, "GET", exposuresPath("demo"), tom, "")
	live, _ := body["live"].([]any)
	if len(live) != 3 {
		t.Fatalf("live = %v", body["live"])
	}
	for _, l := range live {
		e := l.(map[string]any)
		if e["kind"] == nil || e["kind"] == "" {
			t.Errorf("a registry entry without its kind: %v", e)
		}
	}
	for _, entry := range []string{"shares", "dav", "web"} {
		if got := entryOf(t, body, "nextcloud", entry); got["state"] != "approved" {
			t.Errorf("%s = %v, want approved", entry, got)
		}
	}

	// Withdrawn the same way, whatever the kind.
	if code, body := h.do(t, "DELETE", "/v1/tenants/demo/exposures/nextcloud/web", tom, ""); code != http.StatusAccepted {
		t.Fatalf("withdraw = %d %v", code, body)
	}
	_, body = h.do(t, "GET", exposuresPath("demo"), tom, "")
	if got := entryOf(t, body, "nextcloud", "web"); got["state"] != "requested" || got["approval"] != nil {
		t.Fatalf("after the withdrawal = %v", got)
	}
}

// What the approver was shown is what is approved: an approval that names
// another kind than the entry declares is refused and commits nothing.
func TestAnApprovalOfAnotherKindThanTheEntryDeclaresIsRefused(t *testing.T) {
	h := startKinds(t, "app", keepsHeader, nil)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	for path, shown := range map[string]string{
		"/v1/tenants/demo/exposures/nextcloud/dav":    "public",
		"/v1/tenants/demo/exposures/nextcloud/web":    "public",
		"/v1/tenants/demo/exposures/nextcloud/shares": "publicAppCredential",
	} {
		code, body := h.do(t, "PUT", path, tom, `{"kind":"`+shown+`"}`)
		msg, _ := body["error"].(string)
		if code != http.StatusConflict || !strings.Contains(msg, "Nothing was changed") || !strings.Contains(msg, "catalogue entry declares") {
			t.Errorf("%s approved as %s: %d %v", path, shown, code, body)
		}
	}
	// Behind sign-in there is no main address to ask for.
	code, body := h.do(t, "PUT", "/v1/tenants/demo/exposures/nextcloud/web", tom, `{"apex":true,"acknowledgeMainAddressRule":true}`)
	if msg, _ := body["error"].(string); code != http.StatusUnprocessableEntity || !strings.Contains(msg, "is not a public address") {
		t.Errorf("an entry behind sign-in for the main address: %d %v", code, body)
	}
	if h.tip(t) != before {
		t.Fatal("a refused approval committed anyway")
	}
}

// Only the perimeter approver decides, for either kind: the relation asked is
// the one publishing asks, and a member who may see the tenant may not.
func TestOnlyThePerimeterApproverApprovesEitherKind(t *testing.T) {
	h := startKinds(t, "app", keepsHeader, nil)
	mia := h.token(t, "tenant-demo", "mia")
	before := h.tip(t)
	for _, entry := range []string{"web", "dav"} {
		if code, body := h.do(t, "PUT", "/v1/tenants/demo/exposures/nextcloud/"+entry, mia, `{}`); code != http.StatusForbidden {
			t.Errorf("%s approved by somebody who may not publish: %d %v", entry, code, body)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused approval committed anyway")
	}
}

// An approval covers the kind it was given for. When the app's catalogue
// entry comes to ask for something else, the entry is a request again, with
// the earlier approval beside it and the reason said.
func TestAnApprovalDoesNotCoverWhatTheEntryAsksForLater(t *testing.T) {
	path, file := registryFile("demo", "nextcloud dav "+stamp(48*time.Hour), "nextcloud web "+stamp(48*time.Hour))
	// Recorded with no kind, as every approval was: a public address.
	h := startKinds(t, "app", keepsHeader, map[string]string{path: file})
	tom := h.token(t, "tenant-demo", "tom")
	_, body := h.do(t, "GET", exposuresPath("demo"), tom, "")
	for _, entry := range []string{"dav", "web"} {
		got := entryOf(t, body, "nextcloud", entry)
		note, _ := got["note"].(string)
		if got["state"] != "requested" || got["approval"] == nil ||
			!strings.Contains(note, `was approved by olga as "Public address"`) ||
			!strings.Contains(note, "does not cover that") {
			t.Errorf("%s = %v, want a request again with the earlier approval said", entry, got)
		}
	}
	// Approved again, it is recorded as what it is now.
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/exposures/nextcloud/dav", tom, `{}`); code != http.StatusAccepted {
		t.Fatalf("PUT = %d %v", code, body)
	}
	if file := registryOf(t, h, "demo"); !strings.Contains(file, "exposureName: dav\n      kind: publicAppCredential") {
		t.Fatalf("the registry after the new approval:\n%s", file)
	}
	_, body = h.do(t, "GET", exposuresPath("demo"), tom, "")
	if got := entryOf(t, body, "nextcloud", "dav"); got["state"] != "approved" || got["note"] != nil {
		t.Fatalf("after the new approval = %v", got)
	}
}

// An entry behind sign-in that asks for nothing is not listed, and a
// perimeter entry in a mode the platform does not offer is neither listed nor
// approved: nothing would be published for it.
func TestAnEntryInAModeThePlatformDoesNotOfferIsNotApproved(t *testing.T) {
	for _, mode := range []string{"basic", "bearer", "jwt", "signature"} {
		h := startKinds(t, mode, "", nil)
		tom := h.token(t, "tenant-demo", "tom")
		_, body := h.do(t, "GET", exposuresPath("demo"), tom, "")
		if n := len(body["entries"].([]any)); n != 1 {
			t.Fatalf("%s: entries = %v, want the one public address", mode, body["entries"])
		}
		code, body := h.do(t, "PUT", "/v1/tenants/demo/exposures/nextcloud/dav", tom, `{}`)
		msg, _ := body["error"].(string)
		if code != http.StatusUnprocessableEntity || !strings.Contains(msg, "does not offer on a public address") ||
			!strings.Contains(msg, "authMode none or authMode app") {
			t.Errorf("%s: %d %v", mode, code, body)
		}
	}
}
