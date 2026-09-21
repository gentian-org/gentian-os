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

package authn_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/directortest"
)

const audience = "gentian-director"

func verifier(t *testing.T, is *directortest.Issuer) *authn.Verifier {
	t.Helper()
	v, err := authn.NewVerifier(authn.Config{IssuerBase: is.URL, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyAcceptsATokenFromAnyRealmOfThisIssuer(t *testing.T) {
	is := directortest.NewIssuer(t, "gentian", "tenant-demo")
	v := verifier(t, is)
	for _, realm := range []string{"gentian", "tenant-demo"} {
		id, err := v.Verify(context.Background(), is.Token(t, directortest.Claims{
			Realm: realm, Subject: "u-1", Audience: audience, Name: "Ada", Email: "ada@example.com"}))
		if err != nil {
			t.Fatalf("realm %s: %v", realm, err)
		}
		if id.Subject != "u-1" || id.Realm != realm || id.Email != "ada@example.com" || id.SessionID != "sid-u-1" {
			t.Fatalf("identity = %+v", id)
		}
	}
}

func TestVerifyRefuses(t *testing.T) {
	is := directortest.NewIssuer(t, "gentian")
	other := directortest.NewIssuer(t, "gentian")
	v := verifier(t, is)
	ok := directortest.Claims{Realm: "gentian", Subject: "u-1", Audience: audience}

	cases := map[string]string{
		"garbage":            "not-a-token",
		"alg none":           "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJzdWIiOiJ1LTEifQ.",
		"wrong audience":     is.Token(t, with(ok, func(c *directortest.Claims) { c.Audience = "some-other-service" })),
		"expired":            is.Token(t, with(ok, func(c *directortest.Claims) { c.Expiry = time.Now().Add(-time.Hour) })),
		"foreign issuer":     other.Token(t, ok),
		"issuer lookalike":   is.Token(t, with(ok, func(c *directortest.Claims) { c.Issuer = is.URL + ".evil.example/realms/gentian" })),
		"realm traversal":    is.Token(t, with(ok, func(c *directortest.Claims) { c.Issuer = is.URL + "/realms/../../admin" })),
		"realm with a slash": is.Token(t, with(ok, func(c *directortest.Claims) { c.Issuer = is.URL + "/realms/gentian/extra" })),
		"unknown realm":      is.Token(t, with(ok, func(c *directortest.Claims) { c.Realm = "nope" })),
		"unpublished key":    is.Token(t, with(ok, func(c *directortest.Claims) { c.Key = directortest.OtherKey(t) })),
		"unknown key id":     is.Token(t, with(ok, func(c *directortest.Claims) { c.KeyID = "rotated-away" })),
		"no subject":         is.Token(t, with(ok, func(c *directortest.Claims) { c.Subject = "" })),
		"an ID token":        is.Token(t, with(ok, func(c *directortest.Claims) { c.Type = "ID" })),
		"a refresh token":    is.Token(t, with(ok, func(c *directortest.Claims) { c.Type = "Refresh" })),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), raw)
			if !errors.Is(err, authn.ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

// A token signed with a symmetric key derived from the public key is the
// classic confusion attack; the algorithm allow-list is what refuses it.
func TestVerifyRefusesSymmetricAlgorithms(t *testing.T) {
	is := directortest.NewIssuer(t, "gentian")
	v := verifier(t, is)
	hs := "eyJhbGciOiJIUzI1NiIsImtpZCI6InRlc3Qta2V5LTEifQ." +
		"eyJpc3MiOiJ4Iiwic3ViIjoidS0xIn0.c2ln"
	if _, err := v.Verify(context.Background(), hs); !errors.Is(err, authn.ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
}

func TestKeySetIsCachedAndUnknownKeyIDsCannotHammerTheIssuer(t *testing.T) {
	is := directortest.NewIssuer(t, "gentian")
	v := verifier(t, is)
	ok := directortest.Claims{Realm: "gentian", Subject: "u-1", Audience: audience}
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(context.Background(), is.Token(t, ok)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		bad := with(ok, func(c *directortest.Claims) { c.KeyID = "invented" })
		_, _ = v.Verify(context.Background(), is.Token(t, bad))
	}
	if is.Served != 1 {
		t.Fatalf("issuer served its key set %d times, want 1", is.Served)
	}
}

func TestNewVerifierRequiresAnAudience(t *testing.T) {
	_, err := authn.NewVerifier(authn.Config{IssuerBase: "https://id.example"})
	if err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("err = %v", err)
	}
}

func with(c directortest.Claims, f func(*directortest.Claims)) directortest.Claims {
	f(&c)
	return c
}
