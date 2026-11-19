/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package homesync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The realm is asked with a statement it can verify against the keys the
// service publishes, naming the person, the service as issuer and the realm
// as audience, with the scope; the token is kept until shortly before it runs
// out; a person the realm will not answer for is ErrNotLinked.
func TestATokenIsAskedWithAStatementTheRealmCanVerify(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := keys.JWKS()
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		_ = r.ParseForm()
		tok, err := jwt.ParseSigned(r.Form.Get("assertion"), []jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			t.Errorf("statement: %v", err)
		}
		claims := jwt.Claims{}
		if err := tok.Claims(set, &claims); err != nil {
			t.Errorf("the statement does not verify against the published keys: %v", err)
		}
		if claims.Subject == "stranger" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		if claims.Issuer != "https://vouch-home-sync.tenant-acme.svc" || !claims.Audience.Contains("http://kc/realms/acme") ||
			r.Form.Get("scope") != "home-nextcloud-base-ce" || r.Form.Get("client_id") != "vouch-home-sync" {
			t.Errorf("asked with %v, scope %q", claims, r.Form.Get("scope"))
		}
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
	}))
	defer realm.Close()
	tokens := &Tokens{Keys: keys, Vouching: Vouching{
		Issuer: "https://vouch-home-sync.tenant-acme.svc", ClientID: "vouch-home-sync", ClientSecret: "s",
		TokenURL: "http://kc/realms/acme/protocol/openid-connect/token",
	}, Client: &http.Client{Transport: rewrite{to: realm.URL}}}
	// The audience is the realm's issuer, as the token address names it.
	if got := tokens.Vouching.realmIssuer(); got != "http://kc/realms/acme" {
		t.Fatalf("realm issuer %q", got)
	}

	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	tokens.Now = func() time.Time { return now }
	statement, err := tokens.statement("sub-1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := jwt.ParseSigned(statement, []jose.SignatureAlgorithm{jose.RS256})
	claims := jwt.Claims{}
	if err := parsed.Claims(set, &claims); err != nil || claims.Expiry.Time().Sub(claims.IssuedAt.Time()) != statementLife || claims.ID == "" {
		t.Fatalf("statement claims %v (%v)", claims, err)
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		got, err := tokens.For(ctx, "sub-1", "home-nextcloud-base-ce")
		if err != nil || got != "tok" {
			t.Fatalf("token %q (%v)", got, err)
		}
	}
	if asked.Load() != 1 {
		t.Fatalf("the realm was asked %d times for one token", asked.Load())
	}
	now = now.Add(5 * time.Minute)
	if _, err := tokens.For(ctx, "sub-1", "home-nextcloud-base-ce"); err != nil || asked.Load() != 2 {
		t.Fatalf("a token about to run out was not asked again (%d, %v)", asked.Load(), err)
	}
	if _, err := tokens.For(ctx, "stranger", "home-nextcloud-base-ce"); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("a person the realm will not answer for: %v", err)
	}
}

// rewrite sends every request to the test server, keeping the path.
type rewrite struct{ to string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	target, _ := http.NewRequestWithContext(req.Context(), req.Method, r.to+u.Path, req.Body)
	target.Header = req.Header
	return http.DefaultTransport.RoundTrip(target)
}
