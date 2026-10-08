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
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// fakeVerifier knows two kinds of token and keeps them apart, as the real one
// does: an access token is not an ID token and the reverse.
type fakeVerifier struct {
	tokens   map[string]*authn.Identity
	idTokens map[string]map[string]*authn.Identity // client -> raw -> identity
}

func (f fakeVerifier) Verify(_ context.Context, raw string) (*authn.Identity, error) {
	if id, ok := f.tokens[raw]; ok {
		return id, nil
	}
	return nil, authn.ErrUnauthenticated
}

func (f fakeVerifier) VerifyIDToken(_ context.Context, raw, client string) (*authn.Identity, error) {
	if id, ok := f.idTokens[client][raw]; ok {
		return id, nil
	}
	return nil, authn.ErrUnauthenticated
}

type fakeStore struct {
	allow  map[string]bool // "user|relation|object"
	down   bool
	checks int
	log    []authz.Change
}

func (f *fakeStore) Check(_ context.Context, _, user, relation, object string) (bool, error) {
	f.checks++
	if f.down {
		return false, errors.New("connection refused")
	}
	return f.allow[user+"|"+relation+"|"+object], nil
}

func (f *fakeStore) Changes(_ context.Context, _, token string) ([]authz.Change, string, error) {
	if f.down {
		return nil, "", errors.New("connection refused")
	}
	if token == "" {
		return f.log, "end", nil
	}
	return nil, token, nil
}

const kernelClient = "gentian-edge-kernel"

func table() *Table {
	return &Table{Routes: []Route{
		{Host: "argocd.k.example", Relation: "can_configure", Object: "cluster:c1", AuthMode: AuthModeOIDC},
		{Host: "console.k.example", Relation: "can_enter", Object: "tenant:platform", ForwardToken: true, AuthMode: AuthModeOIDC},
		{Host: "id.k.example", Relation: "can_configure", Object: "cluster:c1", KeepClientToken: true, IDTokenAudience: kernelClient, AuthMode: AuthModeOIDC},
		{Host: "api.k.example", Relation: "can_view", Object: "tenant:platform", AuthMode: AuthModeBearer},
		{Host: "shop.k.example", Relation: "can_use", Object: "app:acme/odoo", AuthMode: AuthModeOIDC,
			DenyPaths: []string{"/web/database", "/admin"}},
	}}
}

func decider(store *fakeStore) *Decider {
	root := &authn.Identity{Subject: "root", Realm: "kernel", SessionID: "s1", Email: "root@k.example", Name: "Root"}
	return New(Options{
		Verifier: fakeVerifier{
			tokens: map[string]*authn.Identity{
				"root-token": root,
				// The token the gateway holds after it refreshed root's
				// session: same person, same session, a new token.
				"root-token-refreshed": root,
				"mia-token":            {Subject: "mia", Realm: "kernel", SessionID: "s2"},
				"no-session-token":     {Subject: "root", Realm: "kernel"},
			},
			idTokens: map[string]map[string]*authn.Identity{kernelClient: {
				"root-id-token": root,
				"mia-id-token":  {Subject: "mia", Realm: "kernel", SessionID: "s2"},
			}},
		},
		Store: store, Table: table(), CacheTTL: time.Minute,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func removes(dec Decision, header string) bool {
	for _, h := range dec.RemoveHeaders {
		if h == header {
			return true
		}
	}
	return false
}

// A signed-in request: the gateway's OAuth2 filter validated the session and
// put its access token in the Authorization header. The route's relation
// decides, and the backend gets identity headers in place of the token.
func TestTheRouteRelationDecidesAndIdentityHeadersReplaceTheToken(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	d := decider(store)
	dec := d.Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer root-token"})
	if !dec.Allow {
		t.Fatalf("denied: %s", dec.Reason)
	}
	if dec.Headers[HeaderSubject] != "root" || dec.Headers[HeaderEmail] != "root@k.example" {
		t.Fatalf("headers = %v", dec.Headers)
	}
	if !removes(dec, "authorization") {
		t.Fatalf("a route without forwardToken must strip the bearer; got %v", dec.RemoveHeaders)
	}
	// mia holds no relation: refused, whatever her token says.
	dec = d.Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer mia-token"})
	if dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("mia: allow=%v status=%d", dec.Allow, dec.Status)
	}
}

// Every identity header is set on every allowed request, so one a client sent
// is replaced even where the token has nothing to put in it, and no request
// reaches a backend any other way than through an allow that sets them.
func TestIdentityHeadersAreAlwaysOursNeverTheClients(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:mia|can_configure|cluster:c1": true}}
	dec := decider(store).Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer mia-token"})
	if !dec.Allow {
		t.Fatalf("denied: %s", dec.Reason)
	}
	// mia's token names no email and no display name.
	for _, h := range []string{HeaderSubject, HeaderRealm, HeaderSession, HeaderEmail, HeaderName} {
		if _, set := dec.Headers[h]; !set {
			t.Fatalf("%s is not set, so a client-sent one would reach the backend: %v", h, dec.Headers)
		}
	}
	if dec.Headers[HeaderEmail] != "" || dec.Headers[HeaderName] != "" {
		t.Fatalf("headers = %v", dec.Headers)
	}
	// And a refusal sets nothing and reaches nothing.
	dec = decider(&fakeStore{}).Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer mia-token"})
	if dec.Allow || len(dec.Headers) != 0 {
		t.Fatalf("a refusal carried headers: %+v", dec)
	}
}

func TestTheDesktopRouteKeepsTheToken(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_enter|tenant:platform": true}}
	dec := decider(store).Decide(context.Background(), Request{Host: "console.k.example:443", Authorization: "Bearer root-token"})
	if !dec.Allow {
		t.Fatalf("denied: %s", dec.Reason)
	}
	if removes(dec, "authorization") {
		t.Fatalf("forwardToken route must keep the bearer; removes %v", dec.RemoveHeaders)
	}
}

// The weakness this order closes. A session whose access token has run out
// used to pass this service as "no session yet", be refreshed by the filter
// behind it and go on to the backend unchecked. The filter refreshes first
// now, so what arrives is the new token and it is checked like any other; and
// a token that has run out, should one ever arrive, is refused and never
// waved through.
func TestTheRequestAfterARefreshIsCheckedOnItsNewToken(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	d := decider(store)
	dec := d.Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer root-token-refreshed"})
	if !dec.Allow || dec.Headers[HeaderSubject] != "root" {
		t.Fatalf("the refreshed request: %+v", dec)
	}
	if store.checks != 1 {
		t.Fatalf("the relation was asked %d times on the refreshed request, want 1", store.checks)
	}
	// The same, for someone whose right is gone: the refresh succeeded at
	// the realm and the request is still refused here.
	dec = d.Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer mia-token"})
	if dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("a refreshed session without the relation: %+v", dec)
	}
	// "expired-token" is unknown to the verifier, as an expired one is.
	dec = d.Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer expired-token"})
	if dec.Allow || dec.Status != http.StatusUnauthorized {
		t.Fatalf("an expired token on an oidc route must be refused, not passed on: %+v", dec)
	}
}

// Nothing reaches a backend on a session route without a token verified here.
// Each of these is a request the OAuth2 filter would not have let through
// with the route configured as the operator writes it; if one arrives anyway
// -- the filters in the wrong order, a pass-through somebody configured by
// hand -- the answer is no.
func TestASessionRouteWithoutAVerifiedTokenIsRefused(t *testing.T) {
	// root holds every relation asked below, so each refusal is the token's.
	store := &fakeStore{allow: map[string]bool{
		"user:root|can_configure|cluster:c1": true,
		"user:root|can_use|app:acme/odoo":    true,
	}}
	d := decider(store)
	for name, req := range map[string]Request{
		"no token":                      {Host: "argocd.k.example", Path: "/"},
		"a forged bearer":               {Host: "argocd.k.example", Path: "/", Authorization: "Bearer forged"},
		"a token of another scheme":     {Host: "argocd.k.example", Path: "/", Authorization: "Basic cm9vdDpyb290"},
		"an ID token as the bearer":     {Host: "argocd.k.example", Path: "/", Authorization: "Bearer root-id-token"},
		"an ID token header, no bearer": {Host: "argocd.k.example", Path: "/", IDToken: "root-id-token"},
		"a token naming no session":     {Host: "argocd.k.example", Path: "/", Authorization: "Bearer no-session-token"},
		// The filter answers these two itself. Arriving here means it did
		// not, and they are then paths like any other.
		"the callback":            {Host: "argocd.k.example", Path: "/oauth2/callback?code=c&state=s"},
		"the logout path":         {Host: "argocd.k.example", Path: LogoutPath},
		"another path under it":   {Host: "argocd.k.example", Path: "/oauth2/anything"},
		"a preflight":             {Host: "shop.k.example", Path: "/web"},
		"a health path":           {Host: "shop.k.example", Path: "/healthz"},
		"a fetch with no session": {Host: "shop.k.example", Path: "/web/dataset/call"},
	} {
		dec := d.Decide(context.Background(), req)
		if dec.Allow {
			t.Errorf("%s was allowed on a session route: %+v", name, dec)
			continue
		}
		if dec.Status != http.StatusUnauthorized || dec.Redirect != "" || len(dec.Headers) != 0 {
			t.Errorf("%s: %+v, want a bare 401", name, dec)
		}
	}
	if store.checks != 0 {
		t.Fatalf("the store was asked %d times about requests with no verified token", store.checks)
	}
}

// What has no route class is refused, and a bearer route is what it was: the
// caller's own token, verified, or a 401.
func TestWhatHasNoRouteClassIsRefusedAndABearerRouteIsUnchanged(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_view|tenant:platform": true}}
	d := decider(store)
	if dec := d.Decide(context.Background(), Request{Host: "nothing.k.example", Authorization: "Bearer root-token"}); dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("no class: %+v", dec)
	}
	if dec := d.Decide(context.Background(), Request{Host: "api.k.example"}); dec.Allow || dec.Status != http.StatusUnauthorized {
		t.Fatalf("no token on a bearer route: %+v", dec)
	}
	if dec := d.Decide(context.Background(), Request{Host: "api.k.example", Authorization: "Bearer forged"}); dec.Allow || dec.Status != http.StatusUnauthorized {
		t.Fatalf("forged on a bearer route: %+v", dec)
	}
	// An ID token header means nothing on a bearer route.
	if dec := d.Decide(context.Background(), Request{Host: "api.k.example", IDToken: "root-id-token"}); dec.Allow {
		t.Fatalf("an ID token header opened a bearer route: %+v", dec)
	}
	dec := d.Decide(context.Background(), Request{Host: "api.k.example", Path: "/v1/things", Authorization: "Bearer root-token"})
	if !dec.Allow || dec.Headers[HeaderSubject] != "root" || !removes(dec, "authorization") {
		t.Fatalf("a valid bearer on a bearer route: %+v", dec)
	}
	// The old sign-out alias is a session route's. Here it is a path.
	if dec := d.Decide(context.Background(), Request{Host: "api.k.example", Path: SignOutPath}); dec.Redirect != "" || dec.Status != http.StatusUnauthorized {
		t.Fatalf("sign-out on a bearer route: %+v", dec)
	}
}

// The Keycloak console's Authorization header is the page's own: a token it
// minted for the Admin REST API. This service must neither judge the session
// by it nor take it away. The session is the ID token the gateway hands over.
func TestAKeepClientTokenRouteIsJudgedByTheSessionAndNotByThePagesBearer(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	d := decider(store)
	dec := d.Decide(context.Background(), Request{
		Host: "id.k.example", Path: "/auth/admin/realms",
		Authorization: "Bearer a-token-for-realm-management",
		IDToken:       "root-id-token",
	})
	if !dec.Allow || dec.Headers[HeaderSubject] != "root" {
		t.Fatalf("denied: %+v", dec)
	}
	if removes(dec, "authorization") {
		t.Fatalf("the console's own bearer was stripped; removes %v", dec.RemoveHeaders)
	}
	if !removes(dec, HeaderIDToken) {
		t.Fatalf("the ID token header went on to the backend; removes %v", dec.RemoveHeaders)
	}
	// The relation is asked of the session's person, not the bearer's.
	dec = d.Decide(context.Background(), Request{
		Host: "id.k.example", Path: "/auth/admin/realms",
		Authorization: "Bearer root-token", IDToken: "mia-id-token",
	})
	if dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("mia's session with root's bearer: %+v", dec)
	}
}

// And such a route has no other way in. A bearer is not a session there,
// whoever's it is; an access token is not an ID token; and a route that names
// no client to hold the ID token against refuses everybody.
func TestAKeepClientTokenRouteTakesNothingButTheSessionsIDToken(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	d := decider(store)
	for name, req := range map[string]Request{
		"no session":                    {Host: "id.k.example", Path: "/auth/admin/"},
		"a valid bearer and no session": {Host: "id.k.example", Path: "/auth/admin/", Authorization: "Bearer root-token"},
		"an access token as the ID":     {Host: "id.k.example", Path: "/auth/admin/", IDToken: "root-token"},
		"a forged ID token":             {Host: "id.k.example", Path: "/auth/admin/", IDToken: "forged"},
	} {
		if dec := d.Decide(context.Background(), req); dec.Allow || dec.Status != http.StatusUnauthorized {
			t.Errorf("%s: %+v, want a 401", name, dec)
		}
	}
	tbl := table()
	for i := range tbl.Routes {
		tbl.Routes[i].IDTokenAudience = ""
	}
	d.SetTable(tbl)
	if dec := d.Decide(context.Background(), Request{Host: "id.k.example", IDToken: "root-id-token"}); dec.Allow {
		t.Fatalf("a route with no client to hold the token against let someone in: %+v", dec)
	}
}

// The ID token header is the gateway's to set and only where the route says
// so. Elsewhere a client may send one; it proves nothing and goes no further.
func TestAnIDTokenHeaderIsNeverPassedToABackend(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_enter|tenant:platform": true}}
	dec := decider(store).Decide(context.Background(), Request{
		Host: "console.k.example", Authorization: "Bearer root-token", IDToken: "anything-a-client-sent",
	})
	if !dec.Allow || !removes(dec, HeaderIDToken) {
		t.Fatalf("%+v", dec)
	}
}

// The old sign-out path is an alias for the gateway's logout, which ends the
// edge's session and the realm's. It asks the store nothing and needs no
// token: a session that is refused everywhere must still be able to end.
func TestTheOldSignOutPathIsSentToTheGatewaysLogout(t *testing.T) {
	store := &fakeStore{}
	d := decider(store)
	for name, req := range map[string]Request{
		"signed in":            {Host: "id.k.example", Path: SignOutPath, IDToken: "root-id-token"},
		"refused everywhere":   {Host: "argocd.k.example", Path: SignOutPath, Authorization: "Bearer mia-token"},
		"no token at all":      {Host: "argocd.k.example", Path: SignOutPath},
		"with a query":         {Host: "argocd.k.example", Path: SignOutPath + "?from=desktop"},
		"behind a deny rule":   {Host: "shop.k.example", Path: SignOutPath},
		"on a forwarded route": {Host: "console.k.example:443", Path: SignOutPath},
	} {
		dec := d.Decide(context.Background(), req)
		if dec.Allow || dec.Status != http.StatusFound || dec.Redirect != LogoutPath {
			t.Errorf("%s: %+v, want a 302 to %s", name, dec, LogoutPath)
		}
	}
	if store.checks != 0 {
		t.Fatalf("signing out asked the store %d times", store.checks)
	}
	// Only that path. One that merely starts with it is a path like any other.
	if dec := d.Decide(context.Background(), Request{Host: "argocd.k.example", Path: SignOutPath + "/x"}); dec.Redirect != "" {
		t.Fatalf("%+v", dec)
	}
}

func TestDecisionsAreCachedPerSessionAndEvictedOnChange(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	d := decider(store)
	req := Request{Host: "argocd.k.example", Authorization: "Bearer root-token"}
	d.Decide(context.Background(), req)
	d.Decide(context.Background(), req)
	// One question per decision now, not two: the revocation check is gone
	// with the tuple that backed it.
	if store.checks != 1 {
		t.Fatalf("store asked %d times, want 1 (the second request is a cache hit)", store.checks)
	}
	// The right is taken away and the changelog moves: the cache is evicted
	// and the next request asks again -- and is refused.
	store.allow = map[string]bool{}
	d.Evict()
	if dec := d.Decide(context.Background(), req); dec.Allow {
		t.Fatal("a revoked right survived eviction")
	}
}

func TestFailClosedButCachedAllowsCarry(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	d := decider(store)
	req := Request{Host: "argocd.k.example", Authorization: "Bearer root-token"}
	if dec := d.Decide(context.Background(), req); !dec.Allow {
		t.Fatalf("denied: %s", dec.Reason)
	}
	store.down = true
	if dec := d.Decide(context.Background(), req); !dec.Allow {
		t.Fatalf("a cached allow must carry while the store is down: %s", dec.Reason)
	}
	// A new login waits.
	if dec := d.Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer mia-token"}); dec.Allow || dec.Status != http.StatusServiceUnavailable {
		t.Fatalf("new login while the store is down: %+v", dec)
	}
}

func TestATableRefusesWhatItCannotDecide(t *testing.T) {
	if _, err := ParseTable([]byte("routes:\n- host: a.example\n")); err == nil {
		t.Fatal("a host with no relation is not a route")
	}
	if _, err := ParseTable([]byte("routes:\n- {host: a.example, relation: r, object: o, authMode: oidc}\n- {host: A.example, relation: r, object: o, authMode: oidc}\n")); err == nil {
		t.Fatal("a host listed twice is ambiguous")
	}
	if _, err := ParseTable([]byte("routes:\n- {host: a.example, relation: r, object: o}\n")); err == nil {
		t.Fatal("a route without an L1 mode is not a route")
	}
	tb, err := ParseTable([]byte("routes:\n- {host: A.Example, relation: can_enter, object: tenant:t, authMode: oidc}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if tb.Match("a.example:443") == nil {
		t.Fatal("hosts match case-insensitively and without the port")
	}
}

// A table written before the cookies stopped being read still loads: what it
// says about them is ignored, and nothing in it can make this service read
// one.
func TestATableThatStillNamesCookiesLoadsAndTheyMeanNothing(t *testing.T) {
	tbl, err := ParseTable([]byte(`routes:
- host: argocd.k.example
  relation: can_configure
  object: cluster:c1
  authMode: oidc
  accessTokenCookie: gentian-kernel-access
  idTokenCookie: gentian-kernel-id
  endSessionURL: https://id.k.example/auth/realms/kernel/protocol/openid-connect/logout
`))
	if err != nil {
		t.Fatal(err)
	}
	d := decider(&fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}})
	d.SetTable(tbl)
	if dec := d.Decide(context.Background(), Request{Host: "argocd.k.example"}); dec.Allow {
		t.Fatalf("%+v", dec)
	}
}

// A refusal on an oidc route is read by a person, so it carries the page that
// names the way out; on a bearer route it is a program's and stays a status.
func TestARefusedBrowserIsToldHowToLeave(t *testing.T) {
	t.Parallel()
	store := &fakeStore{allow: map[string]bool{}}
	d := decider(store)
	for name, req := range map[string]Request{
		"no relation":   {Host: "argocd.k.example", Path: "/", Authorization: "Bearer mia-token"},
		"token refused": {Host: "argocd.k.example", Path: "/", Authorization: "Bearer forged"},
	} {
		if dec := d.Decide(context.Background(), req); dec.Allow || !dec.Browser {
			t.Fatalf("%s: a refused page must be marked for a browser: allow=%v browser=%v", name, dec.Allow, dec.Browser)
		}
	}
	// A bearer route is a program's: it gets the status and nothing else.
	dec := d.Decide(context.Background(), Request{
		Host: "api.k.example", Path: "/v1/things", Authorization: "Bearer mia-token",
	})
	if dec.Allow || dec.Browser {
		t.Fatalf("an API refusal must stay a bare status: allow=%v browser=%v", dec.Allow, dec.Browser)
	}
}

// denyPaths is a control the CRD promised for a while and nothing applied: a
// component author could list an administrative path and have it served
// anyway. It is refused at L2, before identity is looked at, because the
// profile said the path is not published and who is asking does not enter
// into it.
func TestADeniedPathIsRefusedWhoeverIsAsking(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_use|app:acme/odoo": true}}
	d := decider(store)
	allowed := func(path string) Decision {
		return d.Decide(context.Background(), Request{
			Host: "shop.k.example", Path: path, Authorization: "Bearer root-token",
		})
	}
	// root holds the relation, so every refusal below is the deny rule.
	if dec := allowed("/web"); !dec.Allow {
		t.Fatalf("/web should be served: %s", dec.Reason)
	}
	for _, path := range []string{"/web/database", "/web/database/manager", "/admin", "/admin/"} {
		if dec := allowed(path); dec.Allow || dec.Status != http.StatusForbidden {
			t.Fatalf("%s: allow=%v status=%d, want a 403", path, dec.Allow, dec.Status)
		}
	}
	// Prefixes stop at a segment boundary: a denied /admin does not take
	// /administrators with it.
	for _, path := range []string{"/administrators", "/web/databases"} {
		if dec := allowed(path); !dec.Allow {
			t.Fatalf("%s should be served, deny matched too much: %s", path, dec.Reason)
		}
	}
	// A query string is not part of the path and must not defeat the rule.
	if dec := allowed("/admin?x=1"); dec.Allow {
		t.Fatal("a query string got past the deny rule")
	}
}

// A profile that wrote "admin" means the same thing as one that wrote
// "/admin", and an empty entry must not turn into "/" and refuse the host.
func TestDenyPathsAreNormalisedAndAnEmptyOneIsDropped(t *testing.T) {
	tbl, err := ParseTable([]byte(`routes:
- host: shop.k.example
  relation: can_use
  object: app:acme/odoo
  authMode: oidc
  denyPaths: ["admin", "  ", "/web/database/"]
`))
	if err != nil {
		t.Fatal(err)
	}
	r := tbl.Match("shop.k.example")
	if got := r.DenyPaths; len(got) != 2 || got[0] != "/admin" || got[1] != "/web/database/" {
		t.Fatalf("denyPaths = %v", got)
	}
	for _, path := range []string{"/admin/users", "/web/database"} {
		if !r.Denies(path) {
			t.Fatalf("%s should be denied", path)
		}
	}
	if r.Denies("/") {
		t.Fatal("an empty entry refused the whole host")
	}
}

// A host may ask more than entry to the tenant. The App Store app's asks who
// may install apps there, so a member who may enter the tenant and knows the
// host is refused here, at the edge, and somebody who may install is let
// through with the token the app relays.
func TestAHostThatAsksTheInstallRightRefusesAPlainMember(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{
		"user:root|can_enter|tenant:acme":       true,
		"user:root|can_install_app|tenant:acme": true,
		"user:mia|can_enter|tenant:acme":        true,
	}}
	d := decider(store)
	d.SetTable(&Table{Routes: []Route{
		{Host: "store.acme.k.example", Relation: "can_install_app", Object: "tenant:acme", ForwardToken: true, AuthMode: AuthModeOIDC},
		{Host: "console.acme.k.example", Relation: "can_enter", Object: "tenant:acme", ForwardToken: true, AuthMode: AuthModeOIDC},
	}})
	for _, path := range []string{"/", "/api/v1/context", "/oauth/callback?code=c&state=s"} {
		dec := d.Decide(context.Background(), Request{Host: "store.acme.k.example", Path: path, Authorization: "Bearer mia-token"})
		if dec.Allow || dec.Status != http.StatusForbidden {
			t.Fatalf("a member reached %s: allow=%v status=%d", path, dec.Allow, dec.Status)
		}
		dec = d.Decide(context.Background(), Request{Host: "store.acme.k.example", Path: path, Authorization: "Bearer root-token"})
		if !dec.Allow {
			t.Fatalf("somebody who may install was refused %s: %s", path, dec.Reason)
		}
		if removes(dec, "authorization") {
			t.Fatalf("the App Store app's route must keep the bearer; removes %v", dec.RemoveHeaders)
		}
	}
	// The same member still reaches what entry to the tenant reaches.
	if dec := d.Decide(context.Background(), Request{Host: "console.acme.k.example", Authorization: "Bearer mia-token"}); !dec.Allow {
		t.Fatalf("a member was refused the desktop: %s", dec.Reason)
	}
}
