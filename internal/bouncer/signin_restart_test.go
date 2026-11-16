/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// restartDecider is decider() with the session routes naming their cookies,
// as the operator's table does.
func restartDecider(store *fakeStore) *Decider {
	d := decider(store)
	tbl := table()
	for i := range tbl.Routes {
		if tbl.Routes[i].AuthMode == AuthModeOIDC {
			tbl.Routes[i].SessionCookies = []string{"gentian-kernel-access", "gentian-kernel-id"}
			tbl.Routes[i].SessionCookiePrefixes = []string{"AccessToken-", "IdToken-", "RefreshToken-", "OauthHMAC-", "OauthExpires-", "OauthNonce-", "CodeVerifier-"}
		}
	}
	d.SetTable(tbl)
	return d
}

// The cookies of a session the realm has ended, beside one of the app's own.
// RefreshToken- twice: the policy was made again, and the browser still holds
// the cookie of the one before.
const endedSession = "theme=dark; gentian-kernel-access=old; RefreshToken-a1=old; OauthHMAC-a1=h; RefreshToken-b2=older"

func page(host, path, cookie string) Request {
	return Request{Host: host, Path: path, Cookie: cookie, Method: http.MethodGet, FetchMode: "navigate"}
}

// A page whose session has ended is sent back to sign in, with that session's
// cookies cleared, instead of being told it lacks access. Nothing is asked of
// the store and nothing reaches a backend.
func TestAPageWhoseSessionEndedIsSignedInAgain(t *testing.T) {
	t.Parallel()
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	d := restartDecider(store)
	for name, req := range map[string]Request{
		"no token":                  page("argocd.k.example", "/applications?view=tree", endedSession),
		"a token naming no session": withAuth(page("argocd.k.example", "/applications?view=tree", endedSession), "Bearer no-session-token"),
	} {
		dec := d.Decide(context.Background(), req)
		if dec.Allow || dec.Status != http.StatusFound || dec.Redirect != "/applications?view=tree" || reasonOf(dec) != "session-ended" {
			t.Fatalf("%s: %+v", name, dec)
		}
		cleared := map[string]bool{}
		restart := ""
		for _, c := range dec.SetCookies {
			name, rest, _ := strings.Cut(c, "=")
			if name == RestartCookie {
				restart = c
				continue
			}
			if !strings.HasPrefix(rest, "; Max-Age=0; Path=/;") {
				t.Fatalf("%s: %q does not clear the cookie on every path", name, c)
			}
			cleared[name] = true
		}
		for _, want := range []string{"gentian-kernel-access", "RefreshToken-a1", "OauthHMAC-a1", "RefreshToken-b2"} {
			if !cleared[want] {
				t.Fatalf("%s: %s is not cleared: %v", name, want, dec.SetCookies)
			}
		}
		if cleared["theme"] || len(cleared) != 4 {
			t.Fatalf("%s: cleared %v; only the edge's cookies are its to clear", name, cleared)
		}
		if !strings.HasPrefix(restart, RestartCookie+"=1; Max-Age=120;") || !strings.Contains(restart, "HttpOnly") {
			t.Fatalf("%s: restart marker = %q", name, restart)
		}
	}
	if store.checks != 0 {
		t.Fatalf("the store was asked %d times about requests with no session", store.checks)
	}
}

func withAuth(r Request, auth string) Request {
	r.Authorization = auth
	return r
}

// Everything that is not a page with an ended session keeps the refusal it
// had: a fetch cannot follow the redirect, a request without the edge's
// cookies had no session, a route that takes a token has no sign-in, and a
// page that comes back from a fresh sign-in still without a session has a
// fault another round would not mend.
func TestOnlyAPageWithAnEndedSessionIsSignedInAgain(t *testing.T) {
	t.Parallel()
	d := restartDecider(&fakeStore{allow: map[string]bool{}})
	post := page("argocd.k.example", "/", endedSession)
	post.Method = http.MethodPost
	fetch := page("argocd.k.example", "/api/v1/applications", endedSession)
	fetch.FetchMode = "cors"
	unnamed := page("argocd.k.example", "/", endedSession)
	unnamed.FetchMode = ""
	for name, req := range map[string]Request{
		"a fetch":                   fetch,
		"a form post":               post,
		"no fetch mode":             unnamed,
		"none of the edge's cookie": page("argocd.k.example", "/", "theme=dark"),
		"no cookie at all":          page("argocd.k.example", "/", ""),
		"restarted already":         page("argocd.k.example", "/", endedSession+"; "+RestartCookie+"=1"),
		"a token route":             page("api.k.example", "/v1/things", endedSession),
	} {
		dec := d.Decide(context.Background(), req)
		if dec.Allow || dec.Status != http.StatusUnauthorized || dec.Redirect != "" || len(dec.SetCookies) != 0 {
			t.Errorf("%s: %+v, want the 401", name, dec)
		}
	}
	// A session that is alive is judged as ever: a person refused access is
	// told so, and is not sent round to sign in again.
	if dec := d.Decide(context.Background(), withAuth(page("argocd.k.example", "/", endedSession), "Bearer mia-token")); dec.Status != http.StatusForbidden || dec.Redirect != "" {
		t.Fatalf("mia: %+v", dec)
	}
}

// The marker is the edge's, like the session's cookies: a backend never sees it.
func TestTheRestartMarkerNeverReachesABackend(t *testing.T) {
	t.Parallel()
	d := restartDecider(&fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}})
	dec := d.Decide(context.Background(), withAuth(page("argocd.k.example", "/", "theme=dark; "+RestartCookie+"=1"), "Bearer root-token"))
	if !dec.Allow || dec.Headers[HeaderCookie] != "theme=dark" {
		t.Fatalf("allowed with cookie header %q: %+v", dec.Headers[HeaderCookie], dec)
	}
}

// The way back stays on the host.
func TestTheWayBackStaysOnTheHost(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"/":                    "/",
		"/desktop?tab=apps":    "/desktop?tab=apps",
		"//evil.example/x":     "/",
		`/\evil.example`:       "/",
		"evil.example":         "/",
		"":                     "/",
		"/a\r\nlocation: /x":   "/",
		"/apps/%2F%2Fevil.org": "/apps/%2F%2Fevil.org",
	} {
		if got := samePage(in); got != want {
			t.Errorf("samePage(%q) = %q, want %q", in, got, want)
		}
	}
}

// Envoy is told the redirect and every cookie to set, each its own header.
func TestEnvoyGetsTheRestartAndItsCookies(t *testing.T) {
	t.Parallel()
	s := &Server{Decider: restartDecider(&fakeStore{allow: map[string]bool{}})}
	check := func(cookie string) *authv3.CheckResponse {
		resp, err := s.Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
				Id: "r1", Host: "argocd.k.example", Path: "/applications", Method: http.MethodGet,
				Headers: map[string]string{":authority": "argocd.k.example", HeaderFetchMode: "navigate", "cookie": cookie},
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	denied := check(endedSession).GetDeniedResponse()
	if denied.GetStatus().GetCode() != typev3.StatusCode_Found || denied.GetBody() != "" {
		t.Fatalf("restart = %v", denied)
	}
	var location string
	var cookies int
	for _, h := range denied.GetHeaders() {
		switch h.Header.Key {
		case "location":
			location = h.Header.Value
		case "set-cookie":
			cookies++
			if h.AppendAction != corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD {
				t.Fatalf("set-cookie %q would replace the one before it", h.Header.Value)
			}
		}
	}
	if location != "/applications" || cookies != 5 {
		t.Fatalf("location %q, %d set-cookie headers", location, cookies)
	}
	// Restarted already: the page says the sign-in ended, not that access is missing.
	again := check(endedSession + "; " + RestartCookie + "=1").GetDeniedResponse()
	if again.GetStatus().GetCode() != typev3.StatusCode_Unauthorized ||
		!strings.Contains(again.GetBody(), "Your sign-in has ended") || strings.Contains(again.GetBody(), "not available to you") {
		t.Fatalf("after a restart = %v", again)
	}
}

// The refusal page still says what it said for a person who lacks access.
func TestARefusalForAccessKeepsItsPage(t *testing.T) {
	t.Parallel()
	page := deniedPage("", http.StatusForbidden)
	if !strings.Contains(page, "This is not available to you") || !strings.Contains(page, `href="`+LogoutPath+`"`) {
		t.Fatalf("page = %s", page)
	}
}
