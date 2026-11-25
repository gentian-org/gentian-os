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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
)

// fakeDeviceRealm accepts one device password per username and hands out a
// door token for it with the password's ways.
type fakeDeviceRealm struct {
	offers    bool
	passwords map[string]string // username -> password
	ways      map[string][]string
	tokens    map[string]string // username -> token
	grants    int
}

func (f *fakeDeviceRealm) Offers(string) bool { return f.offers }

func (f *fakeDeviceRealm) Grant(_ context.Context, _, user, password string) (DeviceToken, error) {
	f.grants++
	if f.passwords[user] == "" || f.passwords[user] != password {
		return DeviceToken{}, ErrDeviceRefused
	}
	return DeviceToken{Token: f.tokens[user], Ways: f.ways[user], Expires: time.Now().Add(5 * time.Minute)}, nil
}

func basic(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// A door whose Nextcloud prefix is served by the ways files and calendars,
// and a realm that takes ana's device password for calendars alone.
func deviceDoor(t *testing.T) (*Decider, *fakeDeviceRealm, *fakeExchanger) {
	t.Helper()
	ex := &fakeExchanger{life: 5 * time.Minute}
	d, _ := doorDecider(t, ex)
	route := doorRoute()
	route.Paths[0].Ways = []string{"calendars", "files"}
	route.Paths = append(route.Paths, PathRoute{
		Prefix: "/remote.php/dav/addressbooks", Relation: "can_use", Object: "app:acme/nextcloud",
		ExchangeScope: "app-nextcloud", Ways: []string{"contacts"},
	})
	d.table = &Table{Routes: []Route{route}}
	realm := &fakeDeviceRealm{
		offers:    true,
		passwords: map[string]string{"ana": "k3m9x-p2hrt-aaaaa-bbbbb-ccccc"},
		ways:      map[string][]string{"ana": {"calendars"}},
		tokens:    map[string]string{"ana": "ana-device-token"},
	}
	d.devices = newDevices(realm, time.Now)
	v := d.verify.(fakeVerifier)
	v.entryTokens[doorAudience]["ana-device-token"] = &authn.Identity{Subject: "ana", Realm: "acme", SessionID: "d1", Client: DevicesClientID}
	return d, realm, ex
}

// A device password is turned into the person's token and the request is
// decided as one that presented it: the app's question, and the app handed a
// token of its own.
func TestADevicePasswordReachesTheAppAsThePerson(t *testing.T) {
	d, realm, ex := deviceDoor(t)
	for range 3 {
		dec := d.Decide(context.Background(), Request{
			Host: doorHost, Path: "/remote.php/dav/calendars/ana/", Authorization: basic("ana", "k3m9x-p2hrt-aaaaa-bbbbb-ccccc"),
		})
		if !dec.Allow || dec.Headers["authorization"] != "Bearer app-token-for-app-nextcloud" {
			t.Fatalf("refused or handed the wrong token: %+v", dec)
		}
		if dec.object != "app:acme/nextcloud" || dec.client != DevicesClientID {
			t.Fatalf("the record names %s by %s", dec.object, dec.client)
		}
	}
	if realm.grants != 1 {
		t.Fatalf("the realm was asked %d times for one device's requests", realm.grants)
	}
	if len(ex.calls) != 1 || ex.calls[0] != "acme|ana-device-token|app-nextcloud" {
		t.Fatalf("the exchange was asked %v", ex.calls)
	}
}

// A path no way of the password serves is refused, though the person may use
// the app and the door serves the path.
func TestADevicePasswordReachesOnlyItsWays(t *testing.T) {
	d, _, _ := deviceDoor(t)
	dec := d.Decide(context.Background(), Request{
		Host: doorHost, Path: "/remote.php/dav/addressbooks/ana/", Authorization: basic("ana", "k3m9x-p2hrt-aaaaa-bbbbb-ccccc"),
	})
	if dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("a way the password does not cover: %+v", dec)
	}
}

// A wrong password is answered with both challenges, and a caller who keeps
// guessing is stopped without the realm being asked.
func TestAWrongDevicePasswordIsChallengedAndLimited(t *testing.T) {
	d, realm, _ := deviceDoor(t)
	ask := func(source string) Decision {
		return d.Decide(context.Background(), Request{
			Host: doorHost, Path: "/remote.php/dav/calendars/ana/", Authorization: basic("ana", "wrong"), Source: source,
		})
	}
	dec := ask("192.0.2.1")
	if dec.Allow || dec.Status != http.StatusUnauthorized || !strings.HasPrefix(dec.Challenge, "Bearer ") ||
		dec.PasswordChallenge != `Basic realm="`+doorHost+`", charset="UTF-8"` {
		t.Fatalf("a wrong password: %+v", dec)
	}
	for range deviceFailuresPerUser - 1 {
		ask("192.0.2.1")
	}
	asked := realm.grants
	if dec := ask("192.0.2.2"); dec.Status != http.StatusTooManyRequests {
		t.Fatalf("past the limit: %+v", dec)
	}
	if realm.grants != asked {
		t.Fatal("the realm was asked past the limit")
	}
}

// A door says it takes passwords only where its realm does, and a password
// presented where it does not is no credential at all.
func TestADoorWhoseRealmTakesNoDevicePasswordsAsksForATokenOnly(t *testing.T) {
	d, realm, _ := deviceDoor(t)
	dec := d.Decide(context.Background(), Request{Host: doorHost, Path: "/remote.php/dav/calendars/ana/"})
	if dec.Status != http.StatusUnauthorized || dec.PasswordChallenge == "" {
		t.Fatalf("no credential at a door that takes passwords: %+v", dec)
	}
	realm.offers = false
	dec = d.Decide(context.Background(), Request{Host: doorHost, Path: "/remote.php/dav/calendars/ana/"})
	if dec.Status != http.StatusUnauthorized || dec.PasswordChallenge != "" {
		t.Fatalf("no credential at a door that takes none: %+v", dec)
	}
	dec = d.Decide(context.Background(), Request{
		Host: doorHost, Path: "/remote.php/dav/calendars/ana/", Authorization: basic("ana", "k3m9x-p2hrt-aaaaa-bbbbb-ccccc"),
	})
	if dec.Allow || dec.Status != http.StatusUnauthorized || realm.grants != 0 {
		t.Fatalf("a password where none is taken: %+v, %d grants", dec, realm.grants)
	}
}

// The realm is asked as the door's device client, with its own secret, and
// its token is read for the ways.
func TestRealmDevicesPostsAPasswordGrant(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acme.devices"), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"gentian_ways": "calendars contacts"})
	token := "h." + base64.RawURLEncoding.EncodeToString(claims) + ".s"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/realms/acme/protocol/openid-connect/token" || r.Form.Get("grant_type") != "password" ||
			r.Form.Get("client_id") != DevicesClientID || r.Form.Get("client_secret") != "s3cret" {
			http.Error(w, `{"error":"unexpected"}`, http.StatusBadRequest)
			return
		}
		if r.Form.Get("password") != "good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"` + token + `","expires_in":300}`))
	}))
	defer srv.Close()
	r := &RealmDevices{TokenBase: srv.URL, SecretsDir: dir}
	if !r.Offers("acme") || r.Offers("globex") || r.Offers("../acme") {
		t.Fatal("offers the wrong realms")
	}
	got, err := r.Grant(context.Background(), "acme", "ana", "good")
	if err != nil || got.Token != token || strings.Join(got.Ways, " ") != "calendars contacts" {
		t.Fatalf("granted %+v, %v", got, err)
	}
	if _, err := r.Grant(context.Background(), "acme", "ana", "bad"); err != ErrDeviceRefused {
		t.Fatalf("a refusal reads as %v", err)
	}
}

func TestBasicCredentialsAreRead(t *testing.T) {
	if u, p, ok := basicAuth(basic("ana@acme.example", "a:b")); !ok || u != "ana@acme.example" || p != "a:b" {
		t.Fatalf("read %q %q %v", u, p, ok)
	}
	for _, h := range []string{"", "Bearer x", "Basic !!", basic("", "x"), basic("ana", "")} {
		if _, _, ok := basicAuth(h); ok {
			t.Errorf("%q read as credentials", h)
		}
	}
}

// A discovery address is answered with its redirect before anybody is asked
// who is calling: a program that knows only the door's name finds the app.
func TestADiscoveryAddressRedirectsWithoutACredential(t *testing.T) {
	d, realm, _ := deviceDoor(t)
	route := d.table.Routes[0]
	route.Paths = append(route.Paths, PathRoute{
		Prefix: "/.well-known/caldav", Relation: "can_use", Object: "app:acme/nextcloud",
		ExchangeScope: "app-nextcloud", Ways: []string{"calendars"}, Redirect: "/remote.php/dav/principals/",
	})
	d.table = &Table{Routes: []Route{route}}
	dec := d.Decide(context.Background(), Request{Host: doorHost, Path: "/.well-known/caldav"})
	if dec.Status != http.StatusMovedPermanently || dec.Redirect != "/remote.php/dav/principals/" || dec.Allow {
		t.Fatalf("discovery: %+v", dec)
	}
	if realm.grants != 0 || dec.subject != "" {
		t.Fatalf("somebody was asked: %d grants, subject %q", realm.grants, dec.subject)
	}
}
