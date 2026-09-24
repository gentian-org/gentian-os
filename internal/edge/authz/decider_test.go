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

package authz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

type fakeVerifier struct{ tokens map[string]*authn.Identity }

func (f fakeVerifier) Verify(_ context.Context, raw string) (*authn.Identity, error) {
	if id, ok := f.tokens[raw]; ok {
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

func table() *Table {
	return &Table{Routes: []Route{
		{Host: "argocd.k.example", Relation: "can_configure", Object: "cluster:c1", AccessTokenCookie: "at", AuthMode: AuthModeOIDC},
		{Host: "console.k.example", Relation: "can_enter", Object: "tenant:platform", AccessTokenCookie: "at", ForwardToken: true, AuthMode: AuthModeOIDC},
		{Host: "id.k.example", Relation: "can_configure", Object: "cluster:c1", AccessTokenCookie: "at", IDTokenCookie: "idt", EndSessionURL: "https://id.k.example/auth/realms/kernel/protocol/openid-connect/logout", KeepClientToken: true, AuthMode: AuthModeOIDC},
		{Host: "api.k.example", Relation: "can_view", Object: "tenant:platform", AuthMode: AuthModeBearer},
	}}
}

func decider(store *fakeStore) *Decider {
	return New(Options{
		Verifier: fakeVerifier{tokens: map[string]*authn.Identity{
			"root-token": {Subject: "root", Realm: "kernel", SessionID: "s1", Email: "root@k.example", Name: "Root"},
			"mia-token":  {Subject: "mia", Realm: "kernel", SessionID: "s2"},
		}},
		Store: store, Table: table(), CacheTTL: time.Minute,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestTheRouteRelationDecidesAndIdentityHeadersReplaceTheToken(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	d := decider(store)
	dec := d.Decide(context.Background(), Request{Host: "argocd.k.example", Cookies: map[string]string{"at": "root-token"}})
	if !dec.Allow {
		t.Fatalf("denied: %s", dec.Reason)
	}
	if dec.Headers[HeaderSubject] != "root" || dec.Headers[HeaderEmail] != "root@k.example" {
		t.Fatalf("headers = %v", dec.Headers)
	}
	if len(dec.RemoveHeaders) != 1 || dec.RemoveHeaders[0] != "authorization" {
		t.Fatalf("a route without forwardToken must strip the bearer; got %v", dec.RemoveHeaders)
	}
	// mia holds no relation: refused, whatever her token says.
	dec = d.Decide(context.Background(), Request{Host: "argocd.k.example", Cookies: map[string]string{"at": "mia-token"}})
	if dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("mia: allow=%v status=%d", dec.Allow, dec.Status)
	}
}

func TestTheDesktopRouteKeepsTheToken(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_enter|tenant:platform": true}}
	dec := decider(store).Decide(context.Background(), Request{Host: "console.k.example:443", Authorization: "Bearer root-token"})
	if !dec.Allow {
		t.Fatalf("denied: %s", dec.Reason)
	}
	if len(dec.RemoveHeaders) != 0 {
		t.Fatalf("forwardToken route must keep the bearer; removes %v", dec.RemoveHeaders)
	}
}

// The Keycloak console is authorised from its zone cookie like anything else,
// but the bearer it then sends is its own. Neither flag means the same thing:
// this route is not forwarded the edge's token, and it must still not be
// stripped of the one it has.
func TestAKeepClientTokenRouteIsNotStripped(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	dec := decider(store).Decide(context.Background(), Request{
		Host:    "id.k.example",
		Cookies: map[string]string{"at": "root-token"},
	})
	if !dec.Allow {
		t.Fatalf("denied: %s", dec.Reason)
	}
	if len(dec.RemoveHeaders) != 0 {
		t.Fatalf("the console's own bearer was stripped; removes %v", dec.RemoveHeaders)
	}
}

// Signing out should not take the person through Keycloak asking whether they
// meant it. That page appears for any logout with no id_token_hint, and the
// hint is in the zone's own cookie, so the edge answers with it.
func TestSignOutCarriesTheHintSoKeycloakDoesNotAsk(t *testing.T) {
	dec := decider(&fakeStore{}).Decide(context.Background(), Request{
		Host:    "id.k.example",
		Path:    SignOutPath,
		Cookies: map[string]string{"at": "root-token", "idt": "the-id-token"},
	})
	if dec.Allow || dec.Status != http.StatusFound {
		t.Fatalf("allow=%v status=%d", dec.Allow, dec.Status)
	}
	u, err := url.Parse(dec.Redirect)
	if err != nil {
		t.Fatalf("redirect is not a URL: %q", dec.Redirect)
	}
	if u.Host != "id.k.example" || u.Path != "/auth/realms/kernel/protocol/openid-connect/logout" {
		t.Fatalf("redirect = %s", dec.Redirect)
	}
	q := u.Query()
	if q.Get("id_token_hint") != "the-id-token" {
		t.Errorf("no hint: %s", dec.Redirect)
	}
	// And back to the edge's own logout afterwards, so the zone's cookies go
	// too. The realm session first, then the edge's, because the other order
	// throws the hint away before it has been used.
	if q.Get("post_logout_redirect_uri") != "https://id.k.example/oauth2/logout" {
		t.Errorf("post-logout target = %q", q.Get("post_logout_redirect_uri"))
	}
}

// A session the graph refuses everywhere must still be able to end itself.
// Someone whose account was just deleted is exactly the person who needs to
// sign out, and asking the store first would refuse them.
func TestSignOutAsksTheStoreNothing(t *testing.T) {
	// A store that would deny everything, and errors if consulted.
	store := &fakeStore{allow: map[string]bool{}}
	dec := decider(store).Decide(context.Background(), Request{
		Host:    "argocd.k.example",
		Path:    SignOutPath,
		Cookies: map[string]string{"at": "mia-token", "idt": "mias-id-token"},
	})
	if dec.Status != http.StatusFound || dec.Redirect == "" {
		t.Fatalf("a refused session could not sign out: %+v", dec)
	}
}

// With no hint there is nothing to gain by going to the realm, so it clears
// the edge's cookies and stops there, which is what it did before.
func TestSignOutWithoutAHintFallsBackToTheEdgesOwnLogout(t *testing.T) {
	dec := decider(&fakeStore{}).Decide(context.Background(), Request{
		Host:    "id.k.example",
		Path:    SignOutPath,
		Cookies: map[string]string{"at": "root-token"},
	})
	if dec.Redirect != "https://id.k.example/oauth2/logout" {
		t.Fatalf("redirect = %q", dec.Redirect)
	}
}

// The Host header carries a port on a non-default port, and a
// post_logout_redirect_uri carrying one will not match what was registered.
func TestSignOutDropsThePortFromTheReturnAddress(t *testing.T) {
	dec := decider(&fakeStore{}).Decide(context.Background(), Request{
		Host:    "id.k.example:8443",
		Path:    SignOutPath,
		Cookies: map[string]string{"idt": "the-id-token"},
	})
	u, _ := url.Parse(dec.Redirect)
	if got := u.Query().Get("post_logout_redirect_uri"); got != "https://id.k.example/oauth2/logout" {
		t.Fatalf("post-logout target = %q", got)
	}
}

// The session on an oidc route is the zone's cookie. A bearer in the header
// belongs to the backend, and judging it as the session is how the Keycloak
// console broke: its page holds a token minted for realm-management, the edge
// expects one minted for the director, so the console's own token failed
// verification, the request was treated as having no session, and the header
// was stripped before it reached Keycloak.
func TestAPagesOwnBearerIsNotMistakenForTheSession(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	dec := decider(store).Decide(context.Background(), Request{
		Host:    "id.k.example",
		Cookies: map[string]string{"at": "root-token"},
		// A token this edge cannot verify, because it was not minted for it.
		Authorization: "Bearer a-token-for-a-different-audience",
	})
	if !dec.Allow {
		t.Fatalf("the zone cookie should have authorised this: %s", dec.Reason)
	}
	if !dec.Identified {
		t.Fatal("the session came from the cookie, so the caller is identified")
	}
	for _, h := range dec.RemoveHeaders {
		if h == "authorization" {
			t.Fatal("the page's own bearer was stripped; the backend gets nothing to authenticate")
		}
	}
	if dec.Headers[HeaderSubject] != "root" {
		t.Fatalf("identity headers come from the session, not the page's token: %v", dec.Headers)
	}
}

// And with no session at all, a keepClientToken route still must not have its
// caller's bearer removed: the backend may be able to authenticate it even
// when the edge cannot.
func TestAKeepClientTokenRouteWithNoSessionKeepsTheHeader(t *testing.T) {
	dec := decider(&fakeStore{}).Decide(context.Background(), Request{
		Host:          "id.k.example",
		Authorization: "Bearer the-pages-own-token",
	})
	if !dec.Allow || dec.Identified {
		t.Fatalf("an oidc route with no session passes through unidentified: %+v", dec)
	}
	for _, h := range dec.RemoveHeaders {
		if h == "authorization" {
			t.Fatal("the caller's bearer was stripped")
		}
	}
	// The identity headers still go, or a backend that trusts them trusts a
	// forgery.
	if len(dec.RemoveHeaders) != len(identityHeaders) {
		t.Fatalf("identity headers must still be removed: %v", dec.RemoveHeaders)
	}
}

// A route that does NOT keep the client token still has it stripped when there
// is no session, which is what keeps the edge's token off every other backend.
func TestAnOrdinaryRouteWithNoSessionStillStripsTheHeader(t *testing.T) {
	dec := decider(&fakeStore{}).Decide(context.Background(), Request{
		Host:          "argocd.k.example",
		Authorization: "Bearer something-unverifiable",
	})
	stripped := false
	for _, h := range dec.RemoveHeaders {
		if h == "authorization" {
			stripped = true
		}
	}
	if !stripped {
		t.Fatalf("removes %v", dec.RemoveHeaders)
	}
}

func TestWhatHasNoRouteClassOrNoTokenIsRefused(t *testing.T) {
	d := decider(&fakeStore{})
	if dec := d.Decide(context.Background(), Request{Host: "nothing.k.example", Authorization: "Bearer root-token"}); dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("no class: %+v", dec)
	}
	// On an oidc route a request with no valid session is the OIDC filter's,
	// which runs behind the shim: it passes, with no identity and no bearer.
	for name, req := range map[string]Request{
		"no token": {Host: "argocd.k.example"},
		"forged":   {Host: "argocd.k.example", Authorization: "Bearer forged"},
	} {
		dec := d.Decide(context.Background(), req)
		if !dec.Allow || dec.Identified {
			t.Fatalf("%s on an oidc route: %+v", name, dec)
		}
		if len(dec.Headers) != 0 || len(dec.RemoveHeaders) < 2 || dec.RemoveHeaders[0] != "authorization" {
			t.Fatalf("%s must carry no identity: %+v", name, dec)
		}
	}
	// On a bearer route the same request is refused here.
	if dec := d.Decide(context.Background(), Request{Host: "api.k.example"}); dec.Allow || dec.Status != http.StatusUnauthorized {
		t.Fatalf("no token on a bearer route: %+v", dec)
	}
	if dec := d.Decide(context.Background(), Request{Host: "api.k.example", Authorization: "Bearer forged"}); dec.Allow || dec.Status != http.StatusUnauthorized {
		t.Fatalf("forged on a bearer route: %+v", dec)
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

// Signing out has to survive its own success. Keycloak's back-channel logout
// records the session as revoked before the browser gets back to the path
// that clears the Gateway's cookies, so a revoked session must still reach
// /oauth2/logout. The callback is the same shape from the other side: it
// completes a sign-in that has no session yet.
func TestTheEdgesOwnPathsAreNotOursToRefuse(t *testing.T) {
	// A store that allows nothing and a revoked session: neither may matter.
	store := &fakeStore{allow: map[string]bool{"user:root|revoked|session:s1": true}}
	d := decider(store)
	for _, path := range []string{"/oauth2/logout", "/oauth2/callback"} {
		dec := d.Decide(context.Background(), Request{
			Host: "console.k.example", Path: path,
			Cookies: map[string]string{"at": "root-token"},
		})
		if !dec.Allow {
			t.Errorf("%s was refused: %s", path, dec.Reason)
		}
		if dec.Headers[HeaderSubject] != "" {
			t.Errorf("%s was given an identity this service did not establish", path)
		}
	}
	// The same session on an ordinary path is still refused, or the exception
	// would be a hole rather than a door.
	dec := d.Decide(context.Background(), Request{
		Host: "console.k.example", Path: "/desktop",
		Cookies: map[string]string{"at": "root-token"},
	})
	if dec.Allow {
		t.Error("a revoked session reached the app itself")
	}
}

// A person refused on a page needs a way out, because the way out is behind
// the same refusal: a session naming an account this cluster no longer knows
// is denied everywhere, including the desktop they would sign out from.
func TestARefusedBrowserIsToldHowToLeave(t *testing.T) {
	t.Parallel()
	store := &fakeStore{allow: map[string]bool{}}
	d := decider(store)
	// An oidc route: a person followed a link here.
	dec := d.Decide(context.Background(), Request{
		Host: "argocd.k.example", Path: "/", Cookies: map[string]string{"at": "mia-token"},
	})
	if dec.Allow || !dec.Browser {
		t.Fatalf("a refused page must be marked for a browser: allow=%v browser=%v", dec.Allow, dec.Browser)
	}
	// A bearer route is a program's: it gets the status and nothing else.
	dec = d.Decide(context.Background(), Request{
		Host: "api.k.example", Path: "/v1/things", Authorization: "Bearer mia-token",
	})
	if dec.Allow || dec.Browser {
		t.Fatalf("an API refusal must stay a bare status: allow=%v browser=%v", dec.Allow, dec.Browser)
	}
}
