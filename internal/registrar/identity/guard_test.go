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
	"errors"
	"net/http"
	"testing"
)

// The realm these tests run in: the platform administrators' group, a group
// of the platform's that holds no role, a custom group, one administrator and
// one person who is not.
//

// guardedRealm is the kernel realm with root1, whom the Cluster claim names
// a role holder, and user1, whom it does not.
func guardedRealm(t *testing.T) (*fakeKeycloak, *Client, Realm) {
	t.Helper()
	f, srv := newFake(t)
	f.users["kernel"] = []userRep{
		{ID: "root1", Username: "admin@k.example", Email: "admin@k.example", Enabled: true, EmailVerified: true},
		{ID: "user1", Username: "ada@k.example", Email: "ada@k.example", Enabled: true, EmailVerified: true},
	}
	c := clientProtecting(t, srv, StaticSource{"kernel": {Realm: "kernel", ClientID: ClientID, ClientSecret: "s"}}, "root1")
	r, err := c.Realm("kernel")
	if err != nil {
		t.Fatal(err)
	}
	return f, c, r
}

// writes is every request the fake received that was not a read.
func (f *fakeKeycloak) writes() []recorded {
	var out []recorded
	for _, c := range f.recorded() {
		if c.method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// refused asserts the rule refused and that nothing was written to the realm.
func refused(t *testing.T, f *fakeKeycloak, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrProtected) {
		t.Errorf("%s: got %v, want ErrProtected", what, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("%s: a refused change still wrote %s %s", what, w[0].method, w[0].path)
	}
}

// personActs is every write this package makes to one person.
func personActs(ctx context.Context) map[string]func(*Client, Realm, string) error {
	address, off := "eve@example.com", false
	return map[string]func(*Client, Realm, string) error{
		"update-person (address)": func(c *Client, r Realm, id string) error {
			_, err := c.UpdatePerson(ctx, r, id, PersonUpdate{Email: &address})
			return err
		},
		"update-person (disable)": func(c *Client, r Realm, id string) error {
			_, err := c.UpdatePerson(ctx, r, id, PersonUpdate{Enabled: &off})
			return err
		},
		"remove-person": func(c *Client, r Realm, id string) error { return c.RemovePerson(ctx, r, id) },
		"require-totp":  func(c *Client, r Realm, id string) error { return c.RequireTOTP(ctx, r, id, true, "", "") },
		"remove-totp":   func(c *Client, r Realm, id string) error { return c.RemoveTOTP(ctx, r, id) },
		"send-password-reset": func(c *Client, r Realm, id string) error {
			return c.SendPasswordReset(ctx, r, id, "", "")
		},
		"activate-account": func(c *Client, r Realm, id string) error {
			_, err := c.ActivateAccount(ctx, r, id, address, true, "", "")
			return err
		},
	}
}

func TestAHolderOfAClusterRoleIsNotChangedFromHere(t *testing.T) {
	for name, act := range personActs(context.Background()) {
		f, c, r := guardedRealm(t)
		// An authenticator to remove, so remove-totp reaches its first write.
		f.creds["root1"] = []map[string]string{{"id": "otp1", "type": "otp"}}
		refused(t, f, name+" on a role holder", act(c, r, "root1"))

		// The same act on somebody who holds no role goes through: the rule
		// is about who the person is, not about the act.
		if err := act(c, r, "user1"); err != nil {
			t.Errorf("%s on somebody else: %v", name, err)
		}
		if len(f.writes()) == 0 {
			t.Errorf("%s on somebody else wrote nothing", name)
		}
	}
}

// The activation link is issued outside the admin API, so it is the one write
// that could have gone round a rule placed on admin paths only.
func TestTheActivationLinkOfARoleHolderIsRefusedOnItsOwnPath(t *testing.T) {
	f, c, r := guardedRealm(t)
	resp, err := c.doAt(context.Background(), r, http.MethodPost,
		"/realms/kernel/gentian-activation/users/root1/link", "link", nil, map[string]any{"actions": []string{"UPDATE_PASSWORD"}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	refused(t, f, "the activation link", err)
}

// Role holders that cannot be read refuse every write to a person: letting
// one through would be deciding without knowing.
func TestWithoutTheRoleHoldersNothingIsWrittenToAPerson(t *testing.T) {
	ctx := context.Background()
	f, srv := newFake(t)
	f.users["kernel"] = []userRep{{ID: "user1", Username: "ada@k.example", Enabled: true}}
	for name, holders := range map[string]RoleHolders{
		"an unreadable claim": func(context.Context) ([]string, error) { return nil, errors.New("the claim is away") },
	} {
		c, err := New(Config{BaseURL: srv.URL, Source: StaticSource{"kernel": {Realm: "kernel", ClientID: ClientID, ClientSecret: "s"}},
			PlatformRoleHolders: holders})
		if err != nil {
			t.Fatal(err)
		}
		r, _ := c.Realm("kernel")
		if err := c.RemovePerson(ctx, r, "user1"); !errors.Is(err, ErrGuardUnavailable) {
			t.Errorf("%s: got %v, want ErrGuardUnavailable", name, err)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
	if _, err := New(Config{BaseURL: srv.URL}); err == nil {
		t.Error("a client that is told no role holders would let every write through")
	}
}

// A write to a group, or one that would put a person in a group, has no
// rule: what a person holds is the rights store's.
func TestAWriteWithNoRuleIsRefused(t *testing.T) {
	f, c, r := guardedRealm(t)
	for _, path := range []string{
		"/clients", "/roles/admin", "/identity-provider/instances", "/sessions", "/sessions/s1/x",
		"/groups", "/groups/g1", "/groups/g1/children", "/users/user1/groups/g1", "/users//groups/g1",
	} {
		resp, err := c.do(context.Background(), r, http.MethodPost, path, nil, map[string]any{})
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrUnguarded) {
			t.Errorf("POST %s: got %v, want ErrUnguarded", path, err)
		}
	}
	// A person created into a group.
	resp, err := c.do(context.Background(), r, http.MethodPost, "/users", nil, map[string]any{"username": "x", "groups": []string{"/any"}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrUnguarded) {
		t.Errorf("a person created into a group: got %v, want ErrUnguarded", err)
	}
	// Another realm's path, reached through this realm's token.
	resp, err = c.doAt(context.Background(), r, http.MethodPut, "/admin/realms/other/users/u1", "x", nil, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrUnguarded) {
		t.Errorf("a write into another realm: got %v, want ErrUnguarded", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
}

func TestClassifyReadsEveryWriteThisPackageMakes(t *testing.T) {
	for rel, want := range map[string]change{
		"/admin/realms/kernel/users":                          {},
		"/admin/realms/kernel/users/u1":                       {user: "u1"},
		"/admin/realms/kernel/users/u1/execute-actions-email": {user: "u1"},
		"/admin/realms/kernel/users/u1/credentials/c1":        {user: "u1", credential: "c1"},
		"/realms/kernel/gentian-device-passwords/users/u1":    {user: "u1"},
		"/realms/kernel/gentian-activation/users/u1/link":     {user: "u1"},
		"/admin/realms/kernel/sessions/s1":                    {session: "s1"},
	} {
		got, ok := classify("kernel", rel)
		if !ok || got != want {
			t.Errorf("classify(%s) = %+v, %v; want %+v", rel, got, ok, want)
		}
	}
}

// In a realm several tenants share, a tenant's token lists, reads and writes
// its own people and nobody else: the others are not found, and a write to
// one is refused before the realm hears of it.
func TestAScopedRealmSeesAndTouchesOnlyItsOwnPeople(t *testing.T) {
	ctx := context.Background()
	f, c, r := guardedRealm(t)
	f.users["kernel"] = append(f.users["kernel"],
		userRep{ID: "mine1", Username: "mia@acme.example", Enabled: true, EmailVerified: true})
	scoped := r.Scoped(only("mine1"))

	people, err := c.People(ctx, scoped, "", 0)
	if err != nil || len(people) != 1 || people[0].ID != "mine1" {
		t.Fatalf("people = %+v, err = %v, want the tenant's own alone", people, err)
	}
	if all, err := c.People(ctx, r, "", 0); err != nil || len(all) != 3 {
		t.Errorf("the unscoped realm lists %d people, err = %v", len(all), err)
	}
	if _, err := c.Person(ctx, scoped, "user1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant's person: got %v, want ErrNotFound", err)
	}
	if p, err := c.Person(ctx, scoped, "mine1"); err != nil || p.ID != "mine1" {
		t.Errorf("the tenant's own person: %+v, %v", p, err)
	}
	for name, act := range personActs(ctx) {
		before := len(f.writes())
		if err := act(c, scoped, "user1"); !errors.Is(err, ErrOutOfScope) && !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on another tenant's person: got %v, want ErrOutOfScope", name, err)
		}
		if len(f.writes()) != before {
			t.Errorf("%s on another tenant's person was written", name)
		}
	}
	if err := c.RemovePerson(ctx, scoped, "mine1"); err != nil {
		t.Errorf("removing the tenant's own person: %v", err)
	}
}

// A scope that cannot be read answers nothing and writes nothing: it is not
// read as the whole realm.
func TestAScopeThatCannotBeReadIsNotTheWholeRealm(t *testing.T) {
	ctx := context.Background()
	f, c, r := guardedRealm(t)
	scoped := r.Scoped(func(context.Context) (map[string]bool, error) { return nil, errors.New("the store is away") })
	if _, err := c.People(ctx, scoped, "", 0); !errors.Is(err, ErrScopeUnavailable) {
		t.Errorf("people: got %v, want ErrScopeUnavailable", err)
	}
	if _, err := c.Person(ctx, scoped, "user1"); !errors.Is(err, ErrScopeUnavailable) {
		t.Errorf("person: got %v, want ErrScopeUnavailable", err)
	}
	if err := c.RemovePerson(ctx, scoped, "user1"); !errors.Is(err, ErrScopeUnavailable) {
		t.Errorf("remove: got %v, want ErrScopeUnavailable", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
}

// An invitation in a shared realm creates the account and mails it, although
// nobody could have given the new account the tenant's position yet.
func TestAnInvitationInASharedRealmMailsThePersonItMade(t *testing.T) {
	f, c, r := guardedRealm(t)
	p, err := c.Invite(context.Background(), r.Scoped(only()), Invitation{Email: "new@acme.example"})
	if err != nil || p.ID != "abc123" {
		t.Fatalf("Invite: %+v, %v", p, err)
	}
	if len(f.callsTo(http.MethodPut, "/users/abc123/execute-actions-email")) != 1 {
		t.Errorf("the invitation was not mailed: %+v", f.writes())
	}
}
