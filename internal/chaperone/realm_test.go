/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const realmIssuer = "https://id.example.org/realms/acme"

var (
	realmKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	otherKey, _ = rsa.GenerateKey(rand.Reader, 2048)
)

// realm serves the realm's keys at .../certs and counts the fetches.
func realm(t *testing.T, fetches *atomic.Int32) *RealmPersons {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/acme/protocol/openid-connect/certs" {
			t.Errorf("keys asked at %s", r.URL.Path)
		}
		if fetches != nil {
			fetches.Add(1)
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &realmKey.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	}))
	t.Cleanup(srv.Close)
	tokenURL := srv.URL + "/realms/acme/protocol/openid-connect/token"
	return NewRealmPersons(realmIssuer, "vouch-chaperone", CertsURL(tokenURL), nil)
}

func realmToken(t *testing.T, key *rsa.PrivateKey, kid string, over map[string]any) string {
	t.Helper()
	now := time.Now().Unix()
	claims := map[string]any{
		"iss": realmIssuer, "sub": "anna-id",
		"aud": []string{"https://mcp.acme.example.org", "vouch-chaperone"},
		"azp": "token-entry-mcp-gateway", "typ": "Bearer", "iat": now, "exp": now + 300,
	}
	for k, v := range over {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestATokenOfTheRealmMadeOutToTheChaperoneNamesPersonAndProgram(t *testing.T) {
	token := realmToken(t, realmKey, "k1", nil)
	who, err := realm(t, nil).Verify(context.Background(), token, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if who.Person != "anna-id" || who.Client != "token-entry-mcp-gateway" || who.Token != token {
		t.Fatalf("present %+v", who)
	}
}

func TestATokenThatSaysAnythingElseIsRefused(t *testing.T) {
	cases := map[string]map[string]any{
		"not made out to the chaperone": {"aud": []string{"https://mcp.acme.example.org"}},
		"another realm":                 {"iss": "https://id.example.org/realms/other"},
		"not an access token":           {"typ": "ID"},
		"no program":                    {"azp": nil},
		"no person":                     {"sub": nil},
		"no start":                      {"iat": nil},
		"expired":                       {"exp": time.Now().Unix() - 1},
	}
	for name, over := range cases {
		if _, err := realm(t, nil).Verify(context.Background(), realmToken(t, realmKey, "k1", over), time.Now().Unix()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestATokenSignedByAnotherKeyOrNotAtAllIsRefused(t *testing.T) {
	now := time.Now().Unix()
	enc := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	unsigned := enc(`{"alg":"none"}`) + "." + enc(`{"sub":"anna-id"}`) + "."
	hmac, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(strings.Repeat("x", 32))},
		(&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	symmetric, err := jwt.Signed(hmac).Claims(map[string]any{"sub": "anna-id"}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{
		"another key": realmToken(t, otherKey, "k1", nil),
		"unknown key": realmToken(t, realmKey, "k2", nil),
		"unsigned":    unsigned,
		"symmetric":   symmetric,
		"not a token": "junk",
	} {
		if _, err := realm(t, nil).Verify(context.Background(), token, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTheKeysAreFetchedOnceAndAgainOnlyForAnUnknownKey(t *testing.T) {
	var fetches atomic.Int32
	p := realm(t, &fetches)
	now := time.Now().Unix()
	for range 2 {
		if _, err := p.Verify(context.Background(), realmToken(t, realmKey, "k1", nil), now); err != nil {
			t.Fatal(err)
		}
	}
	if fetches.Load() != 1 {
		t.Fatalf("%d fetches, want 1", fetches.Load())
	}
	if _, err := p.Verify(context.Background(), realmToken(t, realmKey, "k2", nil), now); err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if fetches.Load() != 2 {
		t.Fatalf("%d fetches, want 2", fetches.Load())
	}
}

func exchange(t *testing.T, handler http.HandlerFunc) *RealmCredentials {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewRealmCredentials(srv.URL+"/token", "vouch-chaperone", "the-secret", nil)
}

func TestTheExchangeAsksAsTheChaperonesClientForOneApp(t *testing.T) {
	c := exchange(t, func(w http.ResponseWriter, r *http.Request) {
		id, secret, _ := r.BasicAuth()
		if r.URL.Path != "/token" || id != "vouch-chaperone" || secret != "the-secret" {
			t.Errorf("asked at %s as %s", r.URL.Path, id)
		}
		_ = r.ParseForm()
		want := map[string]string{
			"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
			"subject_token":        "annas-token",
			"subject_token_type":   "urn:ietf:params:oauth:token-type:access_token",
			"requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"scope":                "app-odoo-base-ce",
		}
		if len(r.PostForm) != len(want) {
			t.Errorf("form %v", r.PostForm)
		}
		for k, v := range want {
			if r.PostForm.Get(k) != v {
				t.Errorf("%s = %q, want %q", k, r.PostForm.Get(k), v)
			}
		}
		_, _ = w.Write([]byte(`{"access_token":"odoo-token"}`))
	})
	got, err := c.ForApp(context.Background(), "annas-token", "odoo-base-ce")
	if err != nil || got != "odoo-token" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestNoTokenFromTheRealmIsAnErrorThatSaysWhy(t *testing.T) {
	refused := exchange(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_scope","error_description":"no"}`))
	})
	_, err := refused.ForApp(context.Background(), "t", "odoo-base-ce")
	var adapter *AdapterError
	if !asAdapterError(err, &adapter) || !strings.Contains(adapter.Message, "no token for odoo-base-ce: no") {
		t.Fatalf("error %v", err)
	}
	empty := exchange(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	bad := exchange(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	})
	down := NewRealmCredentials("http://127.0.0.1:1/token", "vouch-chaperone", "the-secret", nil)
	for name, c := range map[string]*RealmCredentials{"empty": empty, "bad gateway": bad, "down": down} {
		if _, err := c.ForApp(context.Background(), "t", "odoo-base-ce"); !asAdapterError(err, &adapter) {
			t.Errorf("%s: error %v", name, err)
		}
	}
}
