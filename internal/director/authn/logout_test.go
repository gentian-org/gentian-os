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
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/directortest"
)

func logoutToken(t *testing.T, is *directortest.Issuer, f func(*directortest.Claims)) string {
	t.Helper()
	c := directortest.Claims{
		Realm: "kernel", Subject: "u1", Audience: "gentian-edge-kernel",
		Extra: map[string]any{
			"typ":    "Logout",
			"sid":    "sess-1",
			"events": map[string]any{authn.BackchannelLogoutEvent: map[string]any{}},
		},
	}
	if f != nil {
		f(&c)
	}
	return is.Token(t, c)
}

// A logout token names no audience of ours and no Bearer typ; what makes it
// believable is the issuer's key, the realm, the events claim and a sid.
func TestVerifyLogoutAcceptsAZoneClientsLogoutToken(t *testing.T) {
	is := directortest.NewIssuer(t, "kernel", "acme")
	v := verifier(t, is)
	got, err := v.VerifyLogout(context.Background(), logoutToken(t, is, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "u1" || got.SessionID != "sess-1" || got.Realm != "kernel" {
		t.Fatalf("logout = %+v", got)
	}
	// Any realm of this issuer: a tenant zone's client logs out through the
	// same endpoint.
	if _, err := v.VerifyLogout(context.Background(), logoutToken(t, is, func(c *directortest.Claims) { c.Realm = "acme" })); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyLogoutRefuses(t *testing.T) {
	is := directortest.NewIssuer(t, "kernel")
	other := directortest.NewIssuer(t, "kernel")
	v := verifier(t, is)
	cases := map[string]func(*directortest.Claims){
		"an access token, which names no logout event": func(c *directortest.Claims) {
			delete(c.Extra, "events")
		},
		"a token with a nonce, which is an id token": func(c *directortest.Claims) {
			c.Extra["nonce"] = "n"
		},
		"a token naming no session": func(c *directortest.Claims) {
			c.Extra["sid"] = ""
		},
		"another issuer's token": func(c *directortest.Claims) {
			c.Issuer = other.URL + "/realms/kernel"
		},
		"a token signed by a key the issuer does not publish": func(c *directortest.Claims) {
			c.Key = directortest.OtherKey(t)
		},
		"an expired token": func(c *directortest.Claims) {
			c.Expiry = time.Now().Add(-time.Hour)
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v.VerifyLogout(context.Background(), logoutToken(t, is, f))
			if !errors.Is(err, authn.ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}
