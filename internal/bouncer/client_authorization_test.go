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
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authn"
)

// An app's entry behind sign-in whose Authorization header is the app's own
// (clientAuthorization: app, approved), and a second host of the same
// component that keeps nothing. Both are in a tenant's zone, whose client is
// not the kernel's.
const tenantClient = "gentian-edge-acme"

func appTable() *Table {
	return &Table{Routes: []Route{
		{Host: "flows.acme.example", Relation: "can_use", Object: "app:acme/flows", AuthMode: AuthModeOIDC,
			KeepClientToken: true, IDTokenAudience: tenantClient,
			SessionCookies: []string{"gentian-acme-access", "gentian-acme-id"}},
		{Host: "hooks.acme.example", Relation: "can_use", Object: "app:acme/flows", AuthMode: AuthModeOIDC,
			IDTokenSession: true, IDTokenAudience: tenantClient},
		{Host: "plain.acme.example", Relation: "can_use", Object: "app:acme/plain", AuthMode: AuthModeOIDC},
	}}
}

func appDecider(store *fakeStore) *Decider {
	d := decider(store)
	d.verify = fakeVerifier{
		tokens: map[string]*authn.Identity{"ada-access-token": {Subject: "ada", Realm: "acme", SessionID: "s9"}},
		idTokens: map[string]map[string]*authn.Identity{
			tenantClient: {
				"ada-id-token": {Subject: "ada", Realm: "acme", SessionID: "s9", Email: "ada@acme.example"},
				"bob-id-token": {Subject: "bob", Realm: "acme", SessionID: "s8"},
			},
			// An ID token of the same realm issued to another client: the
			// kernel zone's here, any app's own in a real realm.
			kernelClient: {"ada-id-token-for-another-client": {Subject: "ada", Realm: "acme", SessionID: "s9"}},
		},
	}
	d.SetTable(appTable())
	return d
}

// The approved entry: the session and the right to use the app decide, as on
// any entry; the app's own bearer goes through untouched; and nothing of the
// platform's goes with it.
func TestAnAppsOwnAuthorizationHeaderIsKeptAndTheSessionStillDecides(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:ada|can_use|app:acme/flows": true}}
	d := appDecider(store)
	dec := d.Decide(context.Background(), Request{
		Host: "flows.acme.example", Path: "/api/v1/flows",
		Authorization: "Bearer the-apps-own-token", IDToken: "ada-id-token",
		Cookie: "gentian-acme-access=A; theme=dark; gentian-acme-id=I",
	})
	if !dec.Allow || dec.Headers[HeaderSubject] != "ada" {
		t.Fatalf("a signed-in person who may use the app was refused: %+v", dec)
	}
	if removes(dec, "authorization") {
		t.Fatalf("the app's own header was removed: %v", dec.RemoveHeaders)
	}
	// No platform token reaches the app: not the ID token header, not the
	// session's cookies, and the Authorization header is not rewritten.
	if !removes(dec, HeaderIDToken) {
		t.Fatalf("the session's ID token went on to the app: %v", dec.RemoveHeaders)
	}
	if _, set := dec.Headers["authorization"]; set {
		t.Fatalf("the Authorization header was set by the platform: %v", dec.Headers)
	}
	if dec.Headers[HeaderCookie] != "theme=dark" {
		t.Fatalf("Cookie toward the app = %q, want the session's cookies gone and the app's kept", dec.Headers[HeaderCookie])
	}

	// The right to use the app is asked of the session's person.
	dec = d.Decide(context.Background(), Request{
		Host: "flows.acme.example", Authorization: "Bearer the-apps-own-token", IDToken: "bob-id-token",
	})
	if dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("somebody who may not use the app got in on its own header: %+v", dec)
	}
}

// Keeping the header is not accepting it. Without the session there is no
// way in, whatever the Authorization header holds: the app's token, a valid
// platform access token, or an ID token issued to some other client.
func TestAnAuthorizationHeaderIsNeverASessionOnSuchAnEntry(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:ada|can_use|app:acme/flows": true}}
	d := appDecider(store)
	for _, host := range []string{"flows.acme.example", "hooks.acme.example"} {
		for name, req := range map[string]Request{
			"the app's own token and no session":         {Authorization: "Bearer the-apps-own-token"},
			"a valid platform access token, no session":  {Authorization: "Bearer ada-access-token"},
			"an access token where the ID token belongs": {Authorization: "Bearer the-apps-own-token", IDToken: "ada-access-token"},
			"an ID token issued to another client":       {Authorization: "Bearer the-apps-own-token", IDToken: "ada-id-token-for-another-client"},
			"a forged ID token":                          {IDToken: "forged"},
			"nothing at all":                             {},
		} {
			req.Host, req.Path = host, "/api/v1/flows"
			if dec := d.Decide(context.Background(), req); dec.Allow || dec.Status != http.StatusUnauthorized {
				t.Errorf("%s, %s: %+v, want a 401", host, name, dec)
			}
		}
	}
	if store.checks != 0 {
		t.Fatalf("the store was asked %d times about a request with no session", store.checks)
	}
}

// Another host of the same component, which did not ask to keep its header
// or was not approved: the session is proved the same way, because the
// component has one policy, and the header is removed as on any entry.
func TestAHostThatKeepsNothingStillHasItsHeaderRemoved(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:ada|can_use|app:acme/flows": true}}
	d := appDecider(store)
	dec := d.Decide(context.Background(), Request{
		Host: "hooks.acme.example", Authorization: "Bearer anything-a-page-sent", IDToken: "ada-id-token",
	})
	if !dec.Allow || dec.Headers[HeaderSubject] != "ada" {
		t.Fatalf("%+v", dec)
	}
	if !removes(dec, "authorization") || !removes(dec, HeaderIDToken) {
		t.Fatalf("a host that keeps nothing passed a header on: removes %v", dec.RemoveHeaders)
	}
}

// And an ordinary entry is untouched by all of it: the header there is the
// edge's, it is what proves the session, and it is removed.
func TestAnOrdinaryEntryIsJudgedAndStrippedAsBefore(t *testing.T) {
	store := &fakeStore{allow: map[string]bool{"user:ada|can_use|app:acme/plain": true}}
	d := appDecider(store)
	dec := d.Decide(context.Background(), Request{Host: "plain.acme.example", Authorization: "Bearer ada-access-token"})
	if !dec.Allow || !removes(dec, "authorization") {
		t.Fatalf("%+v", dec)
	}
	// An ID token header proves nothing there.
	dec = d.Decide(context.Background(), Request{Host: "plain.acme.example", IDToken: "ada-id-token"})
	if dec.Allow {
		t.Fatalf("an ID token header was taken for a session on an ordinary entry: %+v", dec)
	}
}

// The table says both things, and an older reader of it fails closed: a
// table entry that proves its session by ID token and names no client is a
// route nobody passes.
func TestTheTableCarriesTheNewRouteMode(t *testing.T) {
	tbl, err := ParseTable([]byte(`routes:
- {host: flows.acme.example, relation: can_use, object: "app:acme/flows", authMode: oidc, keepClientToken: true, idTokenAudience: gentian-edge-acme}
- {host: hooks.acme.example, relation: can_use, object: "app:acme/flows", authMode: oidc, idTokenSession: true, idTokenAudience: gentian-edge-acme}
`))
	if err != nil {
		t.Fatal(err)
	}
	if !tbl.Routes[0].KeepClientToken || tbl.Routes[0].IDTokenSession || !tbl.Routes[1].IDTokenSession || tbl.Routes[1].KeepClientToken {
		t.Fatalf("%+v", tbl.Routes)
	}
}
