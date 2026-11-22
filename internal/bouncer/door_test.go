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
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

const (
	doorHost     = "sync.acme.example"
	doorAudience = "https://sync.acme.example"
	doorMetadata = "https://sync.acme.example/.well-known/oauth-protected-resource"
)

func doorRoute() Route {
	return Route{
		Host: doorHost, Relation: "can_enter", Object: "tenant:acme", AuthMode: AuthModeBearer,
		Audience: doorAudience, Realm: "acme", ResourceMetadata: doorMetadata,
		Paths: []PathRoute{
			{Prefix: "/remote.php/dav", Relation: "can_use", Object: "app:acme/nextcloud", ExchangeScope: "app-nextcloud"},
			{Prefix: "/remote.php/dav/public", Relation: "can_use", Object: "app:acme/shares"},
			{Prefix: "/contacts", Relation: "can_use", Object: "app:acme/odoo", ExchangeScope: "app-odoo"},
		},
	}
}

// doorDecider has two people of the tenant acme with a token made out to the
// door: ana may use Nextcloud and the shares, ben may use Odoo.
func doorDecider(t *testing.T, ex Exchanger) (*Decider, *fakeStore) {
	t.Helper()
	ana, err := authz.User("ana")
	if err != nil {
		t.Fatal(err)
	}
	ben, err := authz.User("ben")
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{allow: map[string]bool{
		ana + "|can_use|app:acme/nextcloud": true,
		ana + "|can_use|app:acme/shares":    true,
		ben + "|can_use|app:acme/odoo":      true,
		// Nobody is asked the door's own question; were it asked, both
		// would pass it and ben would reach Nextcloud.
		ana + "|can_enter|tenant:acme": true,
		ben + "|can_enter|tenant:acme": true,
	}}
	d := decider(store)
	d.verify = fakeVerifier{entryTokens: map[string]map[string]*authn.Identity{doorAudience: {
		"ana-door-token": {Subject: "ana", Realm: "acme", SessionID: "a1", Client: "token-entry-nextcloud-dav"},
		"ben-door-token": {Subject: "ben", Realm: "acme", SessionID: "b1", Client: "token-entry-nextcloud-dav"},
	}}}
	d.table = &Table{Routes: []Route{doorRoute()}}
	d.exchange = newExchangeCache(ex, time.Now)
	return d, store
}

// Behind a door each app is asked for by its own question, and the app is
// handed a token of the person that is valid at that app alone.
func TestADoorAsksTheQuestionOfTheAppThePathIsUnder(t *testing.T) {
	ex := &fakeExchanger{life: 5 * time.Minute}
	d, _ := doorDecider(t, ex)

	dec := d.Decide(context.Background(), Request{
		Host: doorHost, Path: "/remote.php/dav/files/ana/notes.md?x=1", Authorization: "Bearer ana-door-token", Cookie: "theme=dark",
	})
	if !dec.Allow {
		t.Fatalf("refused: %d %s", dec.Status, dec.Reason)
	}
	if got := dec.Headers["authorization"]; got != "Bearer app-token-for-app-nextcloud" {
		t.Fatalf("the app is handed %q", got)
	}
	if dec.object != "app:acme/nextcloud" || dec.relation != "can_use" || dec.client != "token-entry-nextcloud-dav" {
		t.Fatalf("the record names %s %s by %s", dec.relation, dec.object, dec.client)
	}
	// The token says who is asking; no header says it beside it, and no
	// cookie goes on.
	for _, h := range append(IdentityHeaders(), HeaderCookie) {
		if _, set := dec.Headers[h]; set || !removes(dec, h) {
			t.Errorf("%s reaches the app: set %v, removes %v", h, set, dec.RemoveHeaders)
		}
	}
	if len(ex.calls) != 1 || ex.calls[0] != "acme|ana-door-token|app-nextcloud" {
		t.Fatalf("the realm was asked %v", ex.calls)
	}

	// The same person, the same host, another app: another question.
	dec = d.Decide(context.Background(), Request{Host: doorHost, Path: "/contacts/ana.vcf", Authorization: "Bearer ana-door-token"})
	if dec.Allow || dec.Status != http.StatusForbidden || dec.object != "app:acme/odoo" || dec.Challenge != "" {
		t.Fatalf("an app the person may not use: %+v", dec)
	}
	dec = d.Decide(context.Background(), Request{Host: doorHost, Path: "/contacts/ben.vcf", Authorization: "Bearer ben-door-token"})
	if !dec.Allow || dec.Headers["authorization"] != "Bearer app-token-for-app-odoo" {
		t.Fatalf("the app the person may use: %+v", dec)
	}
	dec = d.Decide(context.Background(), Request{Host: doorHost, Path: "/remote.php/dav/files/ben/", Authorization: "Bearer ben-door-token"})
	if dec.Allow || dec.Status != http.StatusForbidden {
		t.Fatalf("passing the door's own question opens no app: %+v", dec)
	}
}

// An answer is cached for the question it answered. Cached per host it would
// carry a person from the app they may use to every other behind the door.
func TestADoorsCachedAnswerIsForOneApp(t *testing.T) {
	d, store := doorDecider(t, &fakeExchanger{life: 5 * time.Minute})
	for range 2 {
		if dec := d.Decide(context.Background(), Request{Host: doorHost, Path: "/remote.php/dav/", Authorization: "Bearer ana-door-token"}); !dec.Allow {
			t.Fatalf("refused: %+v", dec)
		}
	}
	if store.checks != 1 {
		t.Fatalf("the store was asked %d times for one question", store.checks)
	}
	dec := d.Decide(context.Background(), Request{Host: doorHost, Path: "/contacts/", Authorization: "Bearer ana-door-token"})
	if dec.Allow || dec.cached {
		t.Fatalf("the answer for one app was given for another: %+v", dec)
	}
}

// The longest prefix is the app, and a prefix that asked for no token of its
// own is not handed the door's.
func TestTheLongestPrefixDecidesAndTheDoorsTokenGoesToNoApp(t *testing.T) {
	ex := &fakeExchanger{life: 5 * time.Minute}
	d, _ := doorDecider(t, ex)
	dec := d.Decide(context.Background(), Request{Host: doorHost, Path: "/remote.php/dav/public/x", Authorization: "Bearer ana-door-token"})
	if !dec.Allow || dec.object != "app:acme/shares" {
		t.Fatalf("%+v", dec)
	}
	if _, set := dec.Headers["authorization"]; set || !removes(dec, "authorization") {
		t.Fatalf("the door's token reaches an app: %v / %v", dec.Headers, dec.RemoveHeaders)
	}
	if dec.Headers[HeaderSubject] != "ana" || dec.Headers[HeaderRealm] != "acme" {
		t.Fatalf("the app is not told who is asking: %v", dec.Headers)
	}
	if len(ex.calls) != 0 {
		t.Fatalf("the realm was asked %v", ex.calls)
	}
	// On a segment boundary, never on the letters.
	dec = d.Decide(context.Background(), Request{Host: doorHost, Path: "/remote.php/dav/publications", Authorization: "Bearer ana-door-token"})
	if !dec.Allow || dec.object != "app:acme/nextcloud" {
		t.Fatalf("%+v", dec)
	}
}

// What is under no app's prefix is not served, whoever asks and whatever
// they hold: the store and the verifier are not asked.
func TestADoorServesNothingOutsideItsPaths(t *testing.T) {
	d, store := doorDecider(t, &fakeExchanger{life: 5 * time.Minute})
	for _, path := range []string{
		"", "/", "/index.php", "/remote.php", "/remote.php/davx", "/contactsx", "/status.php",
		// A path that could be read as another one.
		"/remote.php/dav/../../index.php", "/remote.php/dav/./files", "/remote.php//dav/files",
		"/remote.php/dav/%2e%2e/%2e%2e/index.php", "/remote.php/dav%2Ffiles", "/remote.php/dav/a%5Cb",
		"/remote.php/dav/a\\..\\b", "/contacts/..", "remote.php/dav",
	} {
		for _, auth := range []string{"", "Bearer ana-door-token"} {
			dec := d.Decide(context.Background(), Request{Host: doorHost, Path: path, Authorization: auth})
			if dec.Allow || dec.Status != http.StatusNotFound || dec.Challenge != "" {
				t.Errorf("%q with %q: %+v", path, auth, dec)
			}
			if dec.object != "tenant:acme" {
				t.Errorf("%q: the record names %q", path, dec.object)
			}
		}
	}
	if store.checks != 0 {
		t.Fatalf("the store was asked %d times about paths no app serves", store.checks)
	}
}

// Under an app's prefix a request without a token made out to the door is
// challenged as on any route that takes one.
func TestADoorChallengesARequestWithoutItsToken(t *testing.T) {
	d, store := doorDecider(t, &fakeExchanger{life: 5 * time.Minute})
	for name, auth := range map[string]string{
		"no token": "", "a forged token": "Bearer forged", "the edge's session token": "Bearer root-token",
		"a password": "Basic YW5hOnNlY3JldA==",
	} {
		dec := d.Decide(context.Background(), Request{Host: doorHost, Path: "/remote.php/dav/", Authorization: auth})
		if dec.Allow || dec.Status != http.StatusUnauthorized || !strings.Contains(dec.Challenge, `resource_metadata="`+doorMetadata+`"`) {
			t.Errorf("%s: %+v", name, dec)
		}
	}
	if store.checks != 0 {
		t.Fatalf("the store was asked %d times", store.checks)
	}
}

// An app that is to be handed its own token and cannot be is refused: the
// door's token is never what goes on in its place.
func TestADoorFailsClosedWhenTheAppsTokenCannotBeHad(t *testing.T) {
	for name, ex := range map[string]Exchanger{"the realm is down": &fakeExchanger{down: true}, "none configured": nil} {
		d, _ := doorDecider(t, ex)
		dec := d.Decide(context.Background(), Request{Host: doorHost, Path: "/remote.php/dav/", Authorization: "Bearer ana-door-token"})
		if dec.Allow || dec.Status != http.StatusServiceUnavailable {
			t.Errorf("%s: %+v", name, dec)
		}
	}
}

func TestATableRefusesADoorItCannotDecide(t *testing.T) {
	door := func(change func(*Route)) string {
		r := doorRoute()
		change(&r)
		_, err := ParseTable(mustYAML(t, Table{Routes: []Route{r}}))
		if err == nil {
			return ""
		}
		return err.Error()
	}
	if msg := door(func(*Route) {}); msg != "" {
		t.Fatalf("a door: %s", msg)
	}
	for name, c := range map[string]struct {
		change func(*Route)
		want   string
	}{
		"behind a session": {func(r *Route) {
			r.AuthMode, r.Audience, r.Realm, r.ResourceMetadata = AuthModeOIDC, "", "", ""
		}, "only where it takes a token"},
		"the root":                 {func(r *Route) { r.Paths[0].Prefix = "/" }, "below the root"},
		"no prefix":                {func(r *Route) { r.Paths[0].Prefix = " " }, "below the root"},
		"a prefix that climbs":     {func(r *Route) { r.Paths[0].Prefix = "/dav/../admin" }, "reads one way"},
		"a prefix with a query":    {func(r *Route) { r.Paths[0].Prefix = "/dav?x" }, "reads one way"},
		"a relative prefix":        {func(r *Route) { r.Paths[0].Prefix = "dav" }, "below the root"},
		"no question":              {func(r *Route) { r.Paths[0].Object = "" }, "needs relation and object"},
		"two scopes":               {func(r *Route) { r.Paths[0].ExchangeScope = "app-a app-b" }, "exactly one scope"},
		"a prefix twice":           {func(r *Route) { r.Paths[2].Prefix = "/remote.php/dav/" }, "twice"},
		"an exchange for the door": {func(r *Route) { r.ExchangeScope = "app-nextcloud" }, "only behind a session"},
	} {
		if msg := door(c.change); !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q, want %q", name, msg, c.want)
		}
	}
}

func mustYAML(t *testing.T, v any) []byte {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
