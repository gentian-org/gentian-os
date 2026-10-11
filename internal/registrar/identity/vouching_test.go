/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// linkingRealm is the part of Keycloak's admin API the links go through,
// behaving as Keycloak 26 does for a client with the registrar's roles:
//
//   - the identity providers cannot be read (403);
//   - a link is made to any alias, whether or not a provider has it (204),
//     and a second one to the same alias is a conflict (409);
//   - a person's links are listed only where the provider exists;
//   - removing a link that is not there is a 404, as is anything about a
//     person who is not there.
type linkingRealm struct {
	mu sync.Mutex
	// providers is the aliases the realm has an identity provider for.
	providers map[string]bool
	// users is username by id; groups is the group paths a user holds.
	users  map[string]string
	groups map[string][]string
	// links is the stored links, by user id and alias, shown or not.
	links map[string]federatedIdentityRep
	// writes is every request that was not a read.
	writes []string
}

func newLinkingRealm(t *testing.T, protected ...string) (*linkingRealm, *Client, Realm) {
	t.Helper()
	f := &linkingRealm{
		providers: map[string]bool{"vouch-notary": true, "corporate-sso": true},
		users:     map[string]string{"u-mia": "mia@example.com", "u-root": "root@example.com"},
		groups:    map[string][]string{"u-root": {platformAdmins}},
		links:     map[string]federatedIdentityRep{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	if len(protected) == 0 {
		protected = []string{platformAdmins}
	}
	c := clientProtecting(t, srv, StaticSource{"demo": {Realm: "demo", ClientID: "d", ClientSecret: "s"}}, protected...)
	realm, err := c.Realm("demo")
	if err != nil {
		t.Fatal(err)
	}
	return f, c, realm
}

func (f *linkingRealm) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/admin/realms/demo"))
	}
	seg := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/realms/demo/"), "/")
	w.Header().Set("Content-Type", "application/json")
	if seg[0] == "identity-provider" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if seg[0] != "users" || len(seg) < 2 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id := seg[1]
	username, known := f.users[id]
	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"User not found"}`))
		return
	}
	switch {
	case len(seg) == 2 && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(userRep{ID: id, Username: username, Enabled: true})
	case len(seg) == 3 && seg[2] == "groups":
		out := []groupRep{}
		for _, p := range f.groups[id] {
			out = append(out, groupRep{ID: "g-" + p, Name: p, Path: "/" + p})
		}
		_ = json.NewEncoder(w).Encode(out)
	case len(seg) == 3 && seg[2] == "federated-identity" && r.Method == http.MethodGet:
		out := []federatedIdentityRep{}
		for key, l := range f.links {
			if strings.HasPrefix(key, id+"/") && f.providers[l.IdentityProvider] {
				out = append(out, l)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case len(seg) == 4 && seg[2] == "federated-identity" && r.Method == http.MethodPost:
		if _, there := f.links[id+"/"+seg[3]]; there {
			w.WriteHeader(http.StatusConflict)
			return
		}
		var l federatedIdentityRep
		_ = json.Unmarshal(body, &l)
		f.links[id+"/"+seg[3]] = l
		w.WriteHeader(http.StatusNoContent)
	case len(seg) == 4 && seg[2] == "federated-identity" && r.Method == http.MethodDelete:
		if _, there := f.links[id+"/"+seg[3]]; !there {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Link not found"}`))
			return
		}
		delete(f.links, id+"/"+seg[3])
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *linkingRealm) stored() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.links))
	for key := range f.links {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (f *linkingRealm) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

// A person is linked once, under their own id and their username, and
// linking them again writes nothing.
func TestLinkingNamesThePersonByTheirOwnID(t *testing.T) {
	f, c, realm := newLinkingRealm(t)
	ctx := context.Background()
	for range 2 {
		if err := c.LinkVouching(ctx, realm, "u-mia", "notary"); err != nil {
			t.Fatal(err)
		}
	}
	want := federatedIdentityRep{IdentityProvider: "vouch-notary", UserID: "u-mia", UserName: "mia@example.com"}
	if got := f.links["u-mia/vouch-notary"]; got != want {
		t.Fatalf("the link is %+v, want %+v", got, want)
	}
	if w := f.written(); len(w) != 1 || w[0] != "POST /users/u-mia/federated-identity/vouch-notary" {
		t.Fatalf("writes = %v, want the one link", w)
	}
	if linked, err := c.VouchingLinked(ctx, realm, "u-mia", "notary"); err != nil || !linked {
		t.Fatalf("linked = %v, %v", linked, err)
	}
	if err := c.LinkVouching(ctx, realm, "u-nobody", "notary"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("somebody the realm does not have: %v, want ErrNotFound", err)
	}
}

// Keycloak makes a link to an alias no identity provider has. The registrar
// does not leave one: it is refused, and what was made is taken away.
func TestALinkToAnAliasThatIsNotAnIdentityProviderIsNotLeft(t *testing.T) {
	f, c, realm := newLinkingRealm(t)
	ctx := context.Background()
	if err := c.LinkVouching(ctx, realm, "u-mia", "scribe"); !errors.Is(err, ErrNoSuchIssuer) {
		t.Fatalf("linking to what the realm does not have: %v, want ErrNoSuchIssuer", err)
	}
	if left := f.stored(); len(left) != 0 {
		t.Fatalf("a link to nothing was left behind: %v", left)
	}
	for _, name := range []string{"", "Not-A-Label", "a/b", "../users"} {
		if err := c.LinkVouching(ctx, realm, "u-mia", name); !errors.Is(err, ErrNoSuchIssuer) {
			t.Errorf("profile %q: %v, want ErrNoSuchIssuer", name, err)
		}
	}
	if w := f.written(); len(w) != 2 {
		t.Fatalf("writes = %v, want the link and its removal and nothing for a name that is not a profile's", w)
	}
}

// Unlinking takes the link away, is the same answer when there is none, and
// removes a link that is stored and not shown.
func TestUnlinkingIsIdempotent(t *testing.T) {
	f, c, realm := newLinkingRealm(t)
	ctx := context.Background()
	if err := c.LinkVouching(ctx, realm, "u-mia", "notary"); err != nil {
		t.Fatal(err)
	}
	// A link to an alias that has no provider yet: stored, and not listed.
	f.mu.Lock()
	f.links["u-mia/vouch-scribe"] = federatedIdentityRep{IdentityProvider: "vouch-scribe", UserID: "u-mia"}
	f.mu.Unlock()
	for _, profile := range []string{"notary", "notary", "scribe"} {
		if err := c.UnlinkVouching(ctx, realm, "u-mia", profile); err != nil {
			t.Fatalf("unlinking %s: %v", profile, err)
		}
	}
	if left := f.stored(); len(left) != 0 {
		t.Fatalf("still stored: %v", left)
	}
	if linked, err := c.VouchingLinked(ctx, realm, "u-mia", "notary"); err != nil || linked {
		t.Fatalf("linked = %v, %v", linked, err)
	}
	if err := c.UnlinkVouching(ctx, realm, "u-nobody", "notary"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("somebody the realm does not have: %v, want ErrNotFound", err)
	}
}

// Nobody who holds a platform role is linked from here; the link of one who
// was linked before is still taken away. A link to anything that is not a
// vouching component is how that person signs in, and is not removed.
func TestTheHolderOfAPlatformRoleIsNotLinkedAndCanBeUnlinked(t *testing.T) {
	f, c, realm := newLinkingRealm(t)
	ctx := context.Background()
	if err := c.LinkVouching(ctx, realm, "u-root", "notary"); !errors.Is(err, ErrProtected) {
		t.Fatalf("linking a platform administrator: %v, want ErrProtected", err)
	}
	if w := f.written(); len(w) != 0 {
		t.Fatalf("a refused link was written: %v", w)
	}
	f.mu.Lock()
	f.links["u-root/vouch-notary"] = federatedIdentityRep{IdentityProvider: "vouch-notary", UserID: "u-root"}
	f.links["u-root/corporate-sso"] = federatedIdentityRep{IdentityProvider: "corporate-sso", UserID: "ext-1"}
	f.mu.Unlock()
	if err := c.UnlinkVouching(ctx, realm, "u-root", "notary"); err != nil {
		t.Fatalf("unlinking a platform administrator: %v", err)
	}
	if left := f.stored(); len(left) != 1 || left[0] != "u-root/corporate-sso" {
		t.Fatalf("stored = %v, want the sign-in link alone", left)
	}
	// The same write to a provider that is not a vouching component is a
	// write to the person, and is refused for a role holder.
	err := c.call(ctx, realm, http.MethodDelete, linkPath("u-root", "corporate-sso"), nil, nil, nil)
	if !errors.Is(err, ErrProtected) {
		t.Fatalf("removing a role holder's sign-in link: %v, want ErrProtected", err)
	}
	// Somebody held back from the tenant's approvers is not linked either,
	// and is unlinked like anybody.
	held := WithApproversHeld(ctx, "gentian:tenant:demo:perimeter")
	f.mu.Lock()
	f.groups["u-mia"] = []string{"gentian:tenant:demo:perimeter"}
	f.links["u-mia/vouch-notary"] = federatedIdentityRep{IdentityProvider: "vouch-notary", UserID: "u-mia"}
	f.mu.Unlock()
	if err := c.UnlinkVouching(held, realm, "u-mia", "notary"); err != nil {
		t.Fatalf("unlinking an approver: %v", err)
	}
	if err := c.LinkVouching(held, realm, "u-mia", "notary"); !errors.Is(err, ErrApproversOnly) {
		t.Fatalf("linking an approver for a caller who may not approve: %v, want ErrApproversOnly", err)
	}
}
