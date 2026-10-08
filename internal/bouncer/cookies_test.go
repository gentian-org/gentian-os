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
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"
)

// The names a zone's policy gives its cookies and the ones the Gateway's
// filter makes up, as the operator writes them into the table.
var (
	kernelCookies  = []string{"gentian-kernel-access", "gentian-kernel-id"}
	filterPrefixes = []string{"AccessToken-", "IdToken-", "RefreshToken-", "OauthHMAC-", "OauthExpires-", "OauthNonce-", "CodeVerifier-"}
)

func sessionTable() *Table {
	with := func(r Route) Route {
		r.SessionCookies, r.SessionCookiePrefixes = kernelCookies, filterPrefixes
		return r
	}
	return &Table{Routes: []Route{
		with(Route{Host: "argocd.k.example", Relation: "can_configure", Object: "cluster:c1", AuthMode: AuthModeOIDC}),
		with(Route{Host: "desktop.k.example", Relation: "can_enter", Object: "tenant:platform", ForwardToken: true, AuthMode: AuthModeOIDC}),
		with(Route{Host: "id.k.example", Relation: "can_configure", Object: "cluster:c1", KeepClientToken: true, IDTokenAudience: kernelClient, AuthMode: AuthModeOIDC}),
		// A bearer route has no session. Naming cookies on one changes
		// nothing: only a route with a session has them taken out.
		with(Route{Host: "api.k.example", Relation: "can_configure", Object: "cluster:c1", AuthMode: AuthModeBearer}),
		// A session route whose table line names no cookie.
		{Host: "old.k.example", Relation: "can_configure", Object: "cluster:c1", AuthMode: AuthModeOIDC},
	}}
}

func sessionDecider() *Decider {
	d := decider(&fakeStore{allow: map[string]bool{
		"user:root|can_configure|cluster:c1": true, "user:root|can_enter|tenant:platform": true,
	}})
	d.SetTable(sessionTable())
	return d
}

// The header is rewritten to what the app itself owns, and what it owns is
// not touched: not its bytes, not its order, not the odd spacing, the quotes
// or the cookie it sent twice.
func TestTheSessionsCookiesAreTakenOutAndTheAppsAreLeftAsTheyWere(t *testing.T) {
	route := &sessionTable().Routes[0]
	for name, c := range map[string]struct{ in, out string }{
		"between two of the app's": {
			"sid=abc; gentian-kernel-access=eyJ.a.b; theme=dark", "sid=abc; theme=dark"},
		"first": {
			"gentian-kernel-access=eyJ.a.b; sid=abc", "sid=abc"},
		"last": {
			"sid=abc; gentian-kernel-id=eyJ.i.d", "sid=abc"},
		"every one the filter owns": {
			"OauthHMAC-5a1f=h; a=1; OauthExpires-5a1f=1790000000; gentian-kernel-access=A; b=2; gentian-kernel-id=I; RefreshToken-5a1f=R; c=3",
			"a=1; b=2; c=3"},
		"a sign-in in progress, and another policy's leftovers": {
			"OauthNonce-5a1f.f10w=n; CodeVerifier-5a1f.f10w=v; OauthNonce-5a1f=n; RefreshToken-0ld=R; AccessToken-0ld=A; IdToken-0ld=I; mine=1",
			"mine=1"},
		"odd spacing is the app's": {
			"a=1;gentian-kernel-access=A;  b = 2 ;\tc=3", "a=1;  b = 2 ;\tc=3"},
		"quotes, an empty value and a value with an equals sign": {
			`q="a b, c"; gentian-kernel-id=I; e=; b64=YQ==; j={"k":"v"}`, `q="a b, c"; e=; b64=YQ==; j={"k":"v"}`},
		"a cookie sent twice": {
			"sid=1; gentian-kernel-access=A; sid=2; gentian-kernel-access=B; sid=1", "sid=1; sid=2; sid=1"},
		"a piece with no name and an empty piece": {
			"flag; gentian-kernel-access=A;; x=1", "flag;; x=1"},
		"spaces around the session's own name": {
			"x=1;  gentian-kernel-access =A", "x=1"},
	} {
		got, changed := route.withoutSessionCookies(c.in)
		if !changed || got != c.out {
			t.Errorf("%s:\n  in   %q\n  got  %q (changed %v)\n  want %q", name, c.in, got, changed, c.out)
		}
	}
	// What only looks like one of the session's cookies is the app's: a name
	// is the whole name, in its own case, and a value is not a name.
	for _, mine := range []string{
		"sid=abc; theme=dark",
		"xgentian-kernel-access=1; gentian-kernel-access2=2; GENTIAN-KERNEL-ACCESS=3",
		"note=gentian-kernel-access=A",
		"refreshtoken-5a1f=1; MyRefreshToken-5a1f=2; RefreshToken=3; OauthHMAC=4",
		"",
	} {
		if got, changed := route.withoutSessionCookies(mine); changed || got != mine {
			t.Errorf("%q is not the session's and became %q (changed %v)", mine, got, changed)
		}
	}
}

// On every allowed request of a session route, whatever else the route says
// about tokens.
func TestAnAllowedRequestGoesOnWithoutTheSessionsCookies(t *testing.T) {
	d := sessionDecider()
	const cookies = "sid=abc; gentian-kernel-access=root-token; gentian-kernel-id=root-id-token; RefreshToken-5a1f=R; theme=dark"

	plain := d.Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer root-token", Cookie: cookies})
	if !plain.Allow || plain.Headers[HeaderCookie] != "sid=abc; theme=dark" {
		t.Fatalf("an ordinary route: %+v", plain)
	}
	if !removes(plain, "authorization") || removes(plain, HeaderCookie) {
		t.Fatalf("an ordinary route loses its bearer and keeps a Cookie header: %v", plain.RemoveHeaders)
	}
	// The answer is remembered; the cookies of each request are still its own.
	again := d.Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer root-token", Cookie: "gentian-kernel-access=root-token; other=1"})
	if !again.Allow || again.Headers[HeaderCookie] != "other=1" {
		t.Fatalf("a remembered answer: %+v", again)
	}

	// forwardToken hands the backend the bearer, in the Authorization
	// header, and that is all of the session it gets.
	forwarded := d.Decide(context.Background(), Request{Host: "desktop.k.example", Authorization: "Bearer root-token", Cookie: cookies})
	if !forwarded.Allow || removes(forwarded, "authorization") {
		t.Fatalf("a forwardToken route keeps its bearer: %+v", forwarded)
	}
	if forwarded.Headers[HeaderCookie] != "sid=abc; theme=dark" {
		t.Fatalf("a forwardToken route still gets no token cookie: %q", forwarded.Headers[HeaderCookie])
	}

	// The route that keeps the page's own bearer: the page's cookies stay,
	// the edge's go.
	kept := d.Decide(context.Background(), Request{Host: "id.k.example", Authorization: "Bearer the-pages-own", IDToken: "root-id-token",
		Cookie: "KEYCLOAK_IDENTITY=k; gentian-kernel-id=root-id-token; gentian-kernel-access=root-token; AUTH_SESSION_ID=s"})
	if !kept.Allow || removes(kept, "authorization") || kept.Headers[HeaderCookie] != "KEYCLOAK_IDENTITY=k; AUTH_SESSION_ID=s" {
		t.Fatalf("a keepClientToken route: %+v", kept)
	}
}

// With nothing of the app's left there is no Cookie header to send: an empty
// one is not the same thing to every backend.
func TestAHeaderWithNothingLeftIsRemoved(t *testing.T) {
	d := sessionDecider()
	for _, cookies := range []string{
		"gentian-kernel-access=root-token",
		"gentian-kernel-access=root-token; gentian-kernel-id=I; OauthHMAC-5a1f=h; OauthExpires-5a1f=1; RefreshToken-5a1f=R",
		"gentian-kernel-access=root-token; ; ",
	} {
		dec := d.Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer root-token", Cookie: cookies})
		if !dec.Allow || !removes(dec, HeaderCookie) {
			t.Fatalf("%q: the header must be removed: %+v", cookies, dec)
		}
		if _, set := dec.Headers[HeaderCookie]; set {
			t.Fatalf("%q: removed and set at once: %+v", cookies, dec)
		}
	}
}

// Where there is nothing of the session's to take out, the header is not
// mentioned: the request goes on with the one it came with.
func TestACookieHeaderThatIsNotTheSessionsIsNotTouched(t *testing.T) {
	d := sessionDecider()
	untouched := func(name string, dec Decision) {
		t.Helper()
		if !dec.Allow {
			t.Fatalf("%s: %+v", name, dec)
		}
		if _, set := dec.Headers[HeaderCookie]; set || removes(dec, HeaderCookie) {
			t.Fatalf("%s: the Cookie header was touched: %+v", name, dec)
		}
	}
	// A bearer route has no session; a cookie there is the caller's own,
	// whatever it is called.
	untouched("a bearer route", d.Decide(context.Background(), Request{Host: "api.k.example", Authorization: "Bearer root-token",
		Cookie: "gentian-kernel-access=whatever; sid=1"}))
	untouched("a session route, only the app's cookies", d.Decide(context.Background(), Request{Host: "argocd.k.example",
		Authorization: "Bearer root-token", Cookie: "sid=1; theme=dark"}))
	untouched("a session route, no cookie at all", d.Decide(context.Background(), Request{Host: "argocd.k.example",
		Authorization: "Bearer root-token"}))
	// Names are the table's. A line that names none takes none out: this
	// service does not guess what the edge might have called them.
	untouched("a route whose line names no cookie", d.Decide(context.Background(), Request{Host: "old.k.example",
		Authorization: "Bearer root-token", Cookie: "gentian-kernel-access=root-token; sid=1"}))
}

// A cookie is still no way in: the header is looked at only once the request
// is allowed, and only to pass it on.
func TestACookieNeverStandsInForTheToken(t *testing.T) {
	d := sessionDecider()
	dec := d.Decide(context.Background(), Request{Host: "argocd.k.example", Cookie: "gentian-kernel-access=root-token"})
	if dec.Allow {
		t.Fatalf("%+v", dec)
	}
}

// What Envoy is told: the Cookie header among the headers to set, which
// replaces the request's, or among the headers to remove.
func TestEnvoyIsToldToReplaceOrRemoveTheCookieHeader(t *testing.T) {
	s := &Server{Decider: sessionDecider()}
	check := func(cookie string) *authv3.OkHttpResponse {
		t.Helper()
		resp, err := s.Check(context.Background(), &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
				Id: "r1", Host: "argocd.k.example", Path: "/", Headers: map[string]string{
					":authority": "argocd.k.example", "authorization": "Bearer root-token", "cookie": cookie,
				},
			}},
		}})
		if err != nil || resp.Status.Code != int32(codes.OK) {
			t.Fatalf("%v %v", resp, err)
		}
		return resp.GetOkResponse()
	}
	rewritten := check("sid=abc; gentian-kernel-access=root-token")
	var got string
	for _, h := range rewritten.GetHeaders() {
		if h.Header.Key == "cookie" {
			got = h.Header.Value
			// Not appended. For a request header Envoy 1.39 reads the
			// option's older append flag, not its action: unset, the
			// header is set, and a set replaces every value the request
			// had. The action says the same for a reader that looks there.
			if h.GetAppend().GetValue() || //nolint:staticcheck // the field Envoy's ext_authz reads
				h.AppendAction != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
				t.Fatal("the Cookie header must replace the request's, not be added to it")
			}
		}
	}
	if got != "sid=abc" {
		t.Fatalf("cookie = %q", got)
	}
	removed := check("gentian-kernel-access=root-token")
	for _, h := range removed.GetHeaders() {
		if h.Header.Key == "cookie" {
			t.Fatalf("an empty Cookie header was set: %q", h.Header.Value)
		}
	}
	found := false
	for _, h := range removed.GetHeadersToRemove() {
		found = found || h == "cookie"
	}
	if !found {
		t.Fatalf("headers to remove = %v", removed.GetHeadersToRemove())
	}
}

func TestATableRefusesAnEmptySessionCookieName(t *testing.T) {
	for _, field := range []string{"sessionCookies", "sessionCookiePrefixes"} {
		_, err := ParseTable([]byte("routes:\n- host: a.example\n  relation: can_use\n  object: app:a/b\n  authMode: oidc\n  " + field + ": [\"\"]\n"))
		if err == nil {
			t.Fatalf("an empty entry in %s would own nothing, or everything", field)
		}
	}
}
