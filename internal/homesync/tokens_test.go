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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/gentian-org/gentian-os/internal/vouchingkey"
)

// The realm is asked with a statement it can verify against the public half
// of the key the platform issued the service, naming that key and the person, the service as issuer and the realm
// as audience -- by the issuer its discovery document names, which is the
// address a browser reaches it at, not the one the service asks it at --,
// with the scope; the token is kept until shortly before it runs out; a
// refusal is ErrRefused, with the realm's reason.
func TestATokenIsAskedWithAStatementTheRealmCanVerify(t *testing.T) {
	issued, err := vouchingkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	key, err := vouchingkey.Parse(issued)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := key.JWKS()
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	var asked, discovered atomic.Int32
	realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/realms/acme/.well-known/openid-configuration" {
			discovered.Add(1)
			_, _ = w.Write([]byte(`{"issuer":"https://id.acme.example/auth/realms/acme"}`))
			return
		}
		asked.Add(1)
		_ = r.ParseForm()
		tok, err := jwt.ParseSigned(r.Form.Get("assertion"), []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Errorf("statement: %v", err)
		}
		claims := jwt.Claims{}
		if err := tok.Claims(set, &claims); err != nil {
			t.Errorf("the statement does not verify against the issued key: %v", err)
		}
		if claims.Subject == "stranger" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"user not linked"}`))
			return
		}
		if claims.Issuer != "https://vouch-home-sync.tenant-acme.svc" || !claims.Audience.Contains("https://id.acme.example/auth/realms/acme") ||
			r.Form.Get("scope") != "home-nextcloud-base-ce" || r.Form.Get("client_id") != "vouch-home-sync" {
			t.Errorf("asked with %v, scope %q", claims, r.Form.Get("scope"))
		}
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
	}))
	defer realm.Close()
	tokens := &Tokens{Vouching: Vouching{
		Issuer: "https://vouch-home-sync.tenant-acme.svc", Key: key, ClientID: "vouch-home-sync", ClientSecret: "s",
		TokenURL: "http://kc/realms/acme/protocol/openid-connect/token",
	}, Client: &http.Client{Transport: rewrite{to: realm.URL}}}
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	tokens.Now = func() time.Time { return now }
	statement, err := tokens.statement(tokens.Vouching, "sub-1", "https://id.acme.example/auth/realms/acme")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := jwt.ParseSigned(statement, []jose.SignatureAlgorithm{jose.ES256})
	if kid := parsed.Headers[0].KeyID; kid != key.ID() {
		t.Fatalf("the statement names the key %q, and the key is %q", kid, key.ID())
	}
	if _, err := tokens.statement(Vouching{Issuer: "i"}, "sub-1", "a"); err == nil {
		t.Fatal("a statement was signed without a key")
	}
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
	if asked.Load() != 1 || discovered.Load() != 1 {
		t.Fatalf("the realm was asked %d times for one token, its issuer %d times", asked.Load(), discovered.Load())
	}
	now = now.Add(5 * time.Minute)
	if _, err := tokens.For(ctx, "sub-1", "home-nextcloud-base-ce"); err != nil || asked.Load() != 2 {
		t.Fatalf("a token about to run out was not asked again (%d, %v)", asked.Load(), err)
	}
	if _, err := tokens.For(ctx, "stranger", "home-nextcloud-base-ce"); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "user not linked") {
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

// The client and the signing key are read from the mounted Secret each time
// a token is asked for: a value the platform changes while the service runs
// is the one used next, an entry not there yet stops only the asking, and a
// key the platform did not issue is not signed with.
func TestTheClientIsReadWhenATokenIsAskedFor(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("VOUCHING_CLIENT_ID", "vouch-home-sync")
	write("VOUCHING_CLIENT_SECRET", "s1")
	write("VOUCHING_TOKEN_URL", "http://kc/realms/acme/protocol/openid-connect/token")
	if _, err := ReadVouching(dir); err == nil || !strings.Contains(err.Error(), "VOUCHING_ISSUER") {
		t.Fatalf("an issuer not there yet: %v", err)
	}
	write("VOUCHING_ISSUER", "https://vouch-home-sync.tenant-acme.svc\n")
	if _, err := ReadVouching(dir); err == nil || !strings.Contains(err.Error(), "VOUCHING_SIGNING_KEY") {
		t.Fatalf("a signing key not there yet: %v", err)
	}
	write("VOUCHING_SIGNING_KEY", "not a key")
	if _, err := ReadVouching(dir); err == nil || !strings.Contains(err.Error(), "VOUCHING_SIGNING_KEY") {
		t.Fatalf("a key the platform did not issue: %v", err)
	}
	first, _ := vouchingkey.Generate()
	write("VOUCHING_SIGNING_KEY", string(first))
	v, err := ReadVouching(dir)
	if err != nil || v.Issuer != "https://vouch-home-sync.tenant-acme.svc" || v.ClientSecret != "s1" || v.Key == nil {
		t.Fatalf("read %+v (%v)", v, err)
	}
	second, _ := vouchingkey.Generate()
	write("VOUCHING_SIGNING_KEY", string(second))
	if again, err := ReadVouching(dir); err != nil || again.Key.ID() == v.Key.ID() {
		t.Fatalf("a key the platform made again is not the one read next (%v)", err)
	}
	write("VOUCHING_CLIENT_SECRET", "s2")
	tokens := &Tokens{Read: func() (Vouching, error) { return ReadVouching(dir) }}
	if v, err := tokens.settings(); err != nil || v.ClientSecret != "s2" {
		t.Fatalf("after the secret changed: %+v (%v)", v, err)
	}
}
