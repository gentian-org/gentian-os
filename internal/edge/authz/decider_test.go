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

func TestARevokedSessionIsDeniedAtL2(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{
		"user:root|can_configure|cluster:c1": true,
		"user:root|revoked|session:s1":       true,
	}}
	dec := decider(store).Decide(context.Background(), Request{Host: "argocd.k.example", Authorization: "Bearer root-token"})
	if dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("revoked session: %+v", dec)
	}
}

func TestDecisionsAreCachedPerSessionAndEvictedOnChange(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}}
	d := decider(store)
	req := Request{Host: "argocd.k.example", Authorization: "Bearer root-token"}
	d.Decide(context.Background(), req)
	d.Decide(context.Background(), req)
	if store.checks != 2 { // revoked + relation, once
		t.Fatalf("store asked %d times, want 2 (the second request is a cache hit)", store.checks)
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
