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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

type fakeExchanger struct {
	calls []string
	down  bool
	life  time.Duration
}

func (f *fakeExchanger) Exchange(_ context.Context, realm, subject, scope string) (string, time.Time, error) {
	f.calls = append(f.calls, realm+"|"+subject+"|"+scope)
	if f.down {
		return "", time.Time{}, errors.New("connection refused")
	}
	return "app-token-for-" + scope, time.Now().Add(f.life), nil
}

func exchangeDecider(t *testing.T, ex Exchanger, now func() time.Time) *Decider {
	t.Helper()
	root, err := authz.User("root")
	if err != nil {
		t.Fatal(err)
	}
	d := decider(&fakeStore{allow: map[string]bool{root + "|can_use|app:acme/wiki": true, root + "|can_use|app:acme/odoo": true}})
	d.table = &Table{Routes: []Route{
		{Host: "wiki.k.example", Relation: "can_use", Object: "app:acme/wiki", AuthMode: AuthModeOIDC, ExchangeScope: "app-wiki"},
		{Host: "shop.k.example", Relation: "can_use", Object: "app:acme/odoo", AuthMode: AuthModeOIDC},
	}}
	if now == nil {
		now = time.Now
	}
	d.exchange = newExchangeCache(ex, now)
	return d
}

// A route that asks for it hands its backend the app's token in place of the
// session's, and a route that does not is as it was.
func TestAnExchangeRouteHandsOnTheAppsTokenAndNoOther(t *testing.T) {
	ex := &fakeExchanger{life: 5 * time.Minute}
	d := exchangeDecider(t, ex, nil)
	dec := d.Decide(context.Background(), Request{Host: "wiki.k.example", Path: "/", Authorization: "Bearer root-token"})
	if !dec.Allow {
		t.Fatalf("refused: %d %s", dec.Status, dec.Reason)
	}
	if got := dec.Headers["authorization"]; got != "Bearer app-token-for-app-wiki" {
		t.Fatalf("the backend is handed %q", got)
	}
	if removes(dec, "authorization") {
		t.Fatal("the header that was just set is also removed")
	}
	if len(ex.calls) != 1 || ex.calls[0] != "kernel|root-token|app-wiki" {
		t.Fatalf("the realm was asked %v", ex.calls)
	}
	// The token is all the backend is told: no identity header is set, and
	// each is taken out, so one a client sent does not arrive either.
	for _, h := range IdentityHeaders() {
		if _, set := dec.Headers[h]; set {
			t.Errorf("%s is set beside the token", h)
		}
		if !removes(dec, h) {
			t.Errorf("%s is not removed: one the client sent would reach the backend", h)
		}
	}

	plain := d.Decide(context.Background(), Request{Host: "shop.k.example", Path: "/", Authorization: "Bearer root-token"})
	if !plain.Allow || plain.Headers["authorization"] != "" || !removes(plain, "authorization") || len(ex.calls) != 1 || plain.Headers[HeaderSubject] != "root" {
		t.Fatalf("a route that did not ask: %+v, exchanges %v", plain, ex.calls)
	}
}

// The realm is asked once in a token's lifetime, again shortly before the
// token runs out, and separately for another session.
func TestAnExchangedTokenIsKeptUntilItIsAboutToRunOut(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	ex := &fakeExchanger{life: 5 * time.Minute}
	d := exchangeDecider(t, ex, clock)
	ask := func(token string) Decision {
		return d.Decide(context.Background(), Request{Host: "wiki.k.example", Path: "/", Authorization: "Bearer " + token})
	}
	ask("root-token")
	ask("root-token-refreshed") // the same session, after the Gateway refreshed it
	if len(ex.calls) != 1 {
		t.Fatalf("asked %d times within one token's lifetime", len(ex.calls))
	}
	now = now.Add(5*time.Minute - exchangeMargin + time.Second)
	if dec := ask("root-token-refreshed"); !dec.Allow || len(ex.calls) != 2 || !strings.HasSuffix(ex.calls[1], "|root-token-refreshed|app-wiki") {
		t.Fatalf("shortly before the token runs out: %v", ex.calls)
	}
}

// With no exchange to be had the request is refused. It is never let through
// with the session's token, and never with none.
func TestAnExchangeThatFailsRefusesTheRequest(t *testing.T) {
	for name, ex := range map[string]Exchanger{"the realm is down": &fakeExchanger{down: true}, "none configured": nil} {
		d := exchangeDecider(t, ex, nil)
		dec := d.Decide(context.Background(), Request{Host: "wiki.k.example", Path: "/", Authorization: "Bearer root-token"})
		if dec.Allow || dec.Status != http.StatusServiceUnavailable {
			t.Errorf("%s: allow=%v status=%d", name, dec.Allow, dec.Status)
		}
	}
}

func TestTheTableRefusesAnExchangeWhereThereIsNothingToExchange(t *testing.T) {
	for name, entry := range map[string]string{
		"a bearer route":          `{host: a, relation: r, object: o, authMode: bearer, exchangeScope: app-a}`,
		"a forwarding route":      `{host: a, relation: r, object: o, authMode: oidc, forwardToken: true, exchangeScope: app-a}`,
		"a route keeping its own": `{host: a, relation: r, object: o, authMode: oidc, keepClientToken: true, idTokenAudience: c, exchangeScope: app-a}`,
	} {
		if _, err := ParseTable([]byte("routes: [" + entry + "]")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseTable([]byte(`routes: [{host: a, relation: r, object: o, authMode: oidc, exchangeScope: app-a}]`)); err != nil {
		t.Fatal(err)
	}
}

// The exchange at the realm: what is sent, and what is taken for an answer.
func TestRealmExchangerAsksForOneScopeAndTakesNoOtherAnswer(t *testing.T) {
	var form map[string][]string
	answer := `{"access_token":"T","expires_in":300,"scope":"app-wiki"}`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		if r.URL.Path != "/realms/acme/protocol/openid-connect/token" {
			t.Errorf("asked at %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acme"), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ex := &RealmExchanger{TokenBase: srv.URL + "/", SecretsDir: dir}
	ctx := context.Background()

	token, expires, err := ex.Exchange(ctx, "acme", "session-token", "app-wiki")
	if err != nil || token != "T" || time.Until(expires) < 4*time.Minute {
		t.Fatalf("token %q, expires %v, err %v", token, expires, err)
	}
	want := map[string]string{
		"grant_type": "urn:ietf:params:oauth:grant-type:token-exchange", "client_id": ExchangeClientID, "client_secret": "s3cret",
		"subject_token": "session-token", "scope": "app-wiki",
		"subject_token_type":   "urn:ietf:params:oauth:token-type:access_token",
		"requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
	}
	for k, v := range want {
		if len(form[k]) != 1 || form[k][0] != v {
			t.Errorf("%s = %v, want %q", k, form[k], v)
		}
	}

	// A token for more than this app is not this app's token.
	answer = `{"access_token":"T","expires_in":300,"scope":"app-wiki app-files"}`
	if _, _, err := ex.Exchange(ctx, "acme", "session-token", "app-wiki"); err == nil {
		t.Error("a token for two apps was taken")
	}
	answer, status = `{"error":"invalid_request","error_description":"Invalid token"}`, http.StatusBadRequest
	if _, _, err := ex.Exchange(ctx, "acme", "session-token", "app-wiki"); err == nil || !strings.Contains(err.Error(), "Invalid token") {
		t.Errorf("a refusal: %v", err)
	}
	for name, args := range map[string][3]string{
		"a realm with no secret":     {"globex", "session-token", "app-wiki"},
		"a realm that is not a name": {"../acme", "session-token", "app-wiki"},
		"two scopes":                 {"acme", "session-token", "app-wiki app-files"},
		"no session token":           {"acme", "", "app-wiki"},
	} {
		if _, _, err := ex.Exchange(ctx, args[0], args[1], args[2]); err == nil {
			t.Errorf("%s: exchanged", name)
		}
	}
}
