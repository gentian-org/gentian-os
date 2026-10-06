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
	"net/http/httptest"
	"testing"
)

// The realm these tests run in: the platform administrators' group, a group
// beside it, a custom group, one administrator and one person who is not.
//
// The administrators' group is marked custom here although the platform never
// marks it so. That takes the older refusal -- "only a custom group is renamed
// or deleted" -- out of the way, so what these tests see refuse is the rule
// about administrators and nothing that happens to stand in front of it.
func guardedRealm(t *testing.T) (*fakeKeycloak, *Client, Realm) {
	t.Helper()
	f, srv := newFake(t)
	custom := map[string][]string{CustomGroupAttribute: {"true"}}
	f.groups["kernel"] = []groupRep{
		{ID: "g-admin", Name: platformAdmins, Path: "/" + platformAdmins, Attributes: custom},
		{ID: "g-auditor", Name: "gentian:platform:auditor", Path: "/gentian:platform:auditor"},
		{ID: "g-ops", Name: "gentian:platform:ops", Path: "/gentian:platform:ops", Attributes: custom},
	}
	f.users["kernel"] = []userRep{
		{ID: "root1", Username: "admin@k.example", Email: "admin@k.example", Enabled: true, EmailVerified: true},
		{ID: "user1", Username: "ada@k.example", Email: "ada@k.example", Enabled: true, EmailVerified: true},
	}
	f.memberOf = map[string][]string{
		"root1": {"/" + platformAdmins},
		"user1": {"/gentian:platform:ops"},
	}
	return guardedClient(t, f, srv)
}

func guardedClient(t *testing.T, f *fakeKeycloak, srv *httptest.Server) (*fakeKeycloak, *Client, Realm) {
	t.Helper()
	c := clientFor(t, srv, StaticSource{"kernel": {Realm: "kernel", ClientID: ClientID, ClientSecret: "s"}})
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

func TestNobodyIsAddedToOrRemovedFromThePlatformAdministrators(t *testing.T) {
	f, c, r := guardedRealm(t)
	ctx := context.Background()
	refused(t, f, "adding", c.SetMembership(ctx, r, "user1", platformAdmins, true))
	refused(t, f, "removing", c.SetMembership(ctx, r, "root1", platformAdmins, false))
}

func TestNobodyIsInvitedIntoThePlatformAdministrators(t *testing.T) {
	f, c, r := guardedRealm(t)
	_, err := c.Invite(context.Background(), r, Invitation{
		Email: "eve@example.com", Groups: []string{"gentian:platform:ops", platformAdmins},
	})
	refused(t, f, "inviting into the group", err)
}

func TestThePlatformAdministratorsGroupIsNotRenamedDeletedOrRecreated(t *testing.T) {
	f, c, r := guardedRealm(t)
	ctx := context.Background()

	_, err := c.RenameGroup(ctx, r, platformAdmins, "gentian:platform:former-admins")
	refused(t, f, "renaming the group", err)

	refused(t, f, "deleting the group", c.DeleteGroup(ctx, r, platformAdmins))

	_, err = c.CreateGroup(ctx, r, platformAdmins)
	refused(t, f, "creating a group of that name", err)

	_, err = c.RenameGroup(ctx, r, "gentian:platform:ops", platformAdmins)
	refused(t, f, "renaming another group to that name", err)
}

func TestAPlatformAdministratorIsNotChangedFromHere(t *testing.T) {
	ctx := context.Background()
	address, off := "eve@example.com", false
	for name, act := range map[string]func(*Client, Realm, string) error{
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
	} {
		f, c, r := guardedRealm(t)
		// An authenticator to remove, so remove-totp reaches its first write.
		f.creds["root1"] = []map[string]string{{"id": "otp1", "type": "otp"}}
		refused(t, f, name+" on an administrator", act(c, r, "root1"))

		// The same act on somebody who is not an administrator goes through:
		// the rule is about who the person is, not about the act.
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
func TestTheActivationLinkOfAnAdministratorIsRefusedOnItsOwnPath(t *testing.T) {
	f, c, r := guardedRealm(t)
	resp, err := c.doAt(context.Background(), r, http.MethodPost,
		"/realms/kernel/gentian-activation/users/root1/link", "link", nil, map[string]any{"actions": []string{"UPDATE_PASSWORD"}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	refused(t, f, "the activation link", err)
}

func TestAnAdministratorsOtherGroupsMayStillChange(t *testing.T) {
	f, c, r := guardedRealm(t)
	if err := c.SetMembership(context.Background(), r, "root1", "gentian:platform:auditor", true); err != nil {
		t.Fatalf("adding an administrator to another group: %v", err)
	}
	if w := f.writes(); len(w) != 1 || w[0].method != http.MethodPut {
		t.Fatalf("writes = %v, want one PUT", w)
	}
}

func TestTheRealmsOwnSettingsAreNotAboutAnybody(t *testing.T) {
	_, c, r := guardedRealm(t)
	if err := c.SetPasswordPolicy(context.Background(), r, "length(12)"); err != nil {
		t.Fatalf("password policy: %v", err)
	}
}

// A rule that cannot be applied refuses. The alternative is a write judged
// against no group at all, which lets everything through.
func TestWithoutTheGroupsNameNothingIsWritten(t *testing.T) {
	f, srv := newFake(t)
	f.users["kernel"] = []userRep{{ID: "user1", Username: "ada@k.example"}}
	c, err := New(Config{
		BaseURL: srv.URL,
		Source:  StaticSource{"kernel": {Realm: "kernel", ClientID: ClientID, ClientSecret: "s"}},
		Administrators: func(context.Context) ([]string, error) {
			return nil, errors.New("the Cluster claim could not be read")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := c.Realm("kernel")
	ctx := context.Background()
	for name, err := range map[string]error{
		"remove-person":   c.RemovePerson(ctx, r, "user1"),
		"password policy": c.SetPasswordPolicy(ctx, r, "length(12)"),
	} {
		if !errors.Is(err, ErrGuardUnavailable) {
			t.Errorf("%s: got %v, want ErrGuardUnavailable", name, err)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("wrote %v without knowing the group", w)
	}

	if _, err := New(Config{BaseURL: srv.URL}); err == nil {
		t.Error("a client was built with nothing to name the administrators' group")
	}
}

// A write to somewhere this package was never meant to write is refused, so a
// method added later cannot reach, say, a client or a role by forgetting that
// the rule has to know about it.
func TestAWriteWithNoRuleIsRefused(t *testing.T) {
	f, c, r := guardedRealm(t)
	for _, path := range []string{"/clients", "/roles/admin", "/users//groups/g-admin", "/identity-provider/instances"} {
		resp, err := c.do(context.Background(), r, http.MethodPost, path, nil, map[string]any{})
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrUnguarded) {
			t.Errorf("POST %s: got %v, want ErrUnguarded", path, err)
		}
	}
	// Another realm's path, reached through this realm's token.
	resp, err := c.doAt(context.Background(), r, http.MethodPut, "/admin/realms/other/users/u1", "x", nil, nil)
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
		"/admin/realms/kernel":                                {},
		"/admin/realms/kernel/users":                          {},
		"/admin/realms/kernel/users/u1":                       {user: "u1"},
		"/admin/realms/kernel/users/u1/execute-actions-email": {user: "u1"},
		"/admin/realms/kernel/users/u1/credentials/c1":        {user: "u1"},
		"/admin/realms/kernel/users/u1/groups/g1":             {user: "u1", group: "g1", membership: true},
		"/admin/realms/kernel/groups":                         {},
		"/admin/realms/kernel/groups/g1":                      {group: "g1"},
		"/admin/realms/kernel/groups/g1/children":             {group: "g1"},
		"/realms/kernel/gentian-activation/users/u1/link":     {user: "u1"},
	} {
		got, ok := classify("kernel", rel)
		if !ok || got != want {
			t.Errorf("classify(%s) = %+v, %v; want %+v", rel, got, ok, want)
		}
	}
}

// The administrators' group as the platform really keeps it is not marked
// custom, and "only a custom group is renamed or deleted" would refuse it
// first. It is refused as what it is, so the caller hears the reason that
// will not change; any other group of the platform's keeps the older answer.
func TestThePlatformsOwnAdministratorsGroupIsRefusedAsProtectedNotAsNotCustom(t *testing.T) {
	f, srv := newFake(t)
	f.groups["kernel"] = []groupRep{
		{ID: "g-admin", Name: platformAdmins, Path: "/" + platformAdmins},
		{ID: "g-auditor", Name: "gentian:platform:auditor", Path: "/gentian:platform:auditor"},
	}
	_, c, r := guardedClient(t, f, srv)
	ctx := context.Background()

	refused(t, f, "deleting the group", c.DeleteGroup(ctx, r, platformAdmins))
	_, err := c.RenameGroup(ctx, r, platformAdmins, "gentian:platform:former")
	refused(t, f, "renaming the group", err)

	if err := c.DeleteGroup(ctx, r, "gentian:platform:auditor"); !errors.Is(err, ErrNotCustom) {
		t.Errorf("another group of the platform's: got %v, want ErrNotCustom", err)
	}
}
