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
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Who holds a cluster role is not the registrar's to change.
//
// Every cluster role is held by name (spec.platformAccess.roleHolders of
// the Cluster claim), and a person of the platform tenant named there holds
// it with their kernel-realm account: the cluster's administrator, the
// security officer, an auditor. Whoever can take over one of those accounts
// holds that role over the cluster. The registrar manages people for whoever
// the store allows, and that is a far wider set of callers than the people
// who may decide that -- so it refuses to be the way it is done, whoever
// asks.
//
// Who is named a role holder is written on the claim, by the director, and
// nowhere in the realm: there is no group whose membership is the role, so
// the rule is about the accounts alone.
//
// The rule is applied in ONE place: guard, which doAt calls before every
// request that is not a read. Every call this package makes to Keycloak
// leaves through doAt, so a method added later is covered without knowing
// about the rule, and a write whose shape guard does not recognize is refused
// rather than let through.
//
// What is refused:
//
//   - any write to a person who is a platform-tenant role holder: their
//     names, address and whether they may sign in, removing them, their
//     second factor, a mailed password link, an activation link, linking
//     them to a component that vouches for people. Changing a role holder's
//     address and then mailing a reset is the same as replacing them, and a
//     component that could obtain their tokens would be them;
//   - any write to a group: the registrar writes none;
//   - in a realm several tenants share, any write to a person who is not
//     one of the tenant's own (Realm.Scoped).
//
// This is a rule in the registrar's code, not a property of its credential.
// The Keycloak client it authenticates as holds manage-users in the realm,
// and Keycloak would carry out any of the above for it.

// ErrProtected is a write that would change who holds a cluster role.
var ErrProtected = errors.New("the holders of the cluster's roles are not managed here")

// ErrGuardUnavailable means the rule could not be applied: the role holders
// could not be read. The write is refused, because letting it through would
// be deciding without knowing.
var ErrGuardUnavailable = errors.New("the holders of the cluster's roles could not be determined")

// ErrUnguarded is a write whose shape guard has no rule for. It is a mistake
// in this package and never the caller's, and it is refused.
var ErrUnguarded = errors.New("a write the registrar has no rule for")

// RoleHolders answers the subjects of the platform tenant's people the
// Cluster claim names role holders. Asked on every write rather than read
// once: the claim can change, and the rule has to follow it.
type RoleHolders func(ctx context.Context) ([]string, error)

// change is what one request to Keycloak would change, as far as the rule
// cares.
type change struct {
	// user is the person written to, when the write is about one person.
	user string
	// vouching marks a write to a person's link to a component that vouches
	// for people (vouching.go).
	vouching bool
	// credential is the id of a person's credential the write removes:
	// only ever a device password (devicepasswords.go).
	credential string
	// session is the id of a sign-in session the write ends (sessions.go).
	session string
}

// classify reads a request path as a write. ok is false for a path this
// package was never meant to write to.
func classify(realm, rel string) (w change, ok bool) {
	admin := "/admin/realms/" + url.PathEscape(realm)
	activation := "/realms/" + url.PathEscape(realm) + "/gentian-activation/users/"
	devices := "/realms/" + url.PathEscape(realm) + "/gentian-device-passwords/users/"
	switch {
	case strings.HasPrefix(rel, devices):
		rest := strings.TrimPrefix(rel, devices)
		if rest != "" && !strings.Contains(rest, "/") {
			return change{user: unescape(rest)}, true
		}
		return change{}, false
	case strings.HasPrefix(rel, activation):
		rest := strings.Split(strings.TrimPrefix(rel, activation), "/")
		if len(rest) == 2 && rest[0] != "" && rest[1] == "link" {
			return change{user: unescape(rest[0])}, true
		}
		return change{}, false
	case !strings.HasPrefix(rel, admin+"/"):
		return change{}, false
	}
	seg := strings.Split(strings.TrimPrefix(rel, admin+"/"), "/")
	for _, s := range seg {
		if s == "" {
			return change{}, false
		}
	}
	switch seg[0] {
	case "users":
		switch {
		case len(seg) == 1:
			return change{}, true
		case len(seg) >= 3 && seg[2] == "groups":
			// What a person holds is the rights store's: nobody is put in
			// a group from here.
			return change{}, false
		case len(seg) == 4 && seg[2] == "federated-identity" && strings.HasPrefix(unescape(seg[3]), vouchingAliasPrefix):
			return change{user: unescape(seg[1]), vouching: true}, true
		case len(seg) == 4 && seg[2] == "credentials":
			return change{user: unescape(seg[1]), credential: unescape(seg[3])}, true
		default:
			return change{user: unescape(seg[1])}, true
		}
	case "sessions":
		if len(seg) == 2 {
			return change{session: unescape(seg[1])}, true
		}
		return change{}, false
	}
	return change{}, false
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// guard refuses a write that would change who holds a platform role.
//
// Called by doAt for every request that is not a GET, and by nothing else.
func (c *Client) guard(ctx context.Context, r Realm, method, rel string, body any) error {
	w, ok := classify(r.name, rel)
	if !ok {
		return fmt.Errorf("%w: %s %s", ErrUnguarded, method, rel)
	}
	// Taking away a person's link to a component that vouches for them is
	// let through for everybody, the holder of a platform role included. It
	// gives nobody anything: it ends a component's ability to obtain that
	// person's tokens. Refusing it would leave somebody who was linked before
	// they were given a role with a link nothing here could remove. Making
	// the link is a write to the person like any other, and is judged below:
	// nobody is linked from here while they hold a platform role. A link to
	// any other identity provider is how that person signs in, and is not
	// this case in either direction.
	if w.vouching && method == http.MethodDelete {
		return nil
	}
	// Ending a sign-in session is let through for the same reason: it gives
	// nobody anything, and the holder of a platform role signs in again.
	// Whose session it is was settled before (EndSession ends only the
	// caller's own).
	if w.session != "" && method == http.MethodDelete {
		return nil
	}
	// Removing a device password is the same: it ends what the person was
	// given, for everybody, the holder of a platform role included. Whether
	// the credential is one is asked of the realm, not taken from the
	// caller; any other credential (a second factor) is judged below like
	// every write to a person.
	if w.credential != "" && method == http.MethodDelete {
		held, err := c.DevicePasswords(ctx, r, w.user)
		if err != nil {
			return err
		}
		for _, d := range held {
			if d.ID == w.credential {
				return nil
			}
		}
	}
	// A body that asks for groups would put somebody in one at creation.
	if named, err := namedIn(body); err != nil {
		return fmt.Errorf("%w: %s %s: %v", ErrUnguarded, method, rel, err)
	} else if len(named.Groups) > 0 {
		return fmt.Errorf("%w: %s %s names groups", ErrUnguarded, method, rel)
	}
	// In a shared realm, only the tenant's own people are written to.
	if w.user != "" {
		in, err := r.within(ctx, w.user)
		if err != nil {
			return err
		}
		if !in {
			return fmt.Errorf("%w: user %q", ErrOutOfScope, w.user)
		}
	}
	return c.refuseHolder(ctx, w)
}

// refuseHolder is the rule itself: a person of the platform tenant the
// Cluster claim names a role holder is not written to. Their account is the
// role, and the people of the kernel realm are managed by whoever manages
// the platform tenant's people, who must not be able to take over the
// security officer's account, or another administrator's. It fails closed:
// holders that cannot be read refuse the write.
func (c *Client) refuseHolder(ctx context.Context, w change) error {
	if w.user == "" {
		return nil
	}
	if c.holders == nil {
		return ErrGuardUnavailable
	}
	subjects, err := c.holders(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrGuardUnavailable, err)
	}
	for _, sub := range subjects {
		if sub == w.user {
			return fmt.Errorf("%w: this person holds a cluster role by name", ErrProtected)
		}
	}
	return nil
}

// names is the part of a request body the rule reads.
type names struct {
	Groups []string `json:"groups"`
}

// namedIn reads a body's group list. A body that is not an object -- the
// list of actions a mail carries -- names none.
func namedIn(body any) (names, error) {
	var out names
	if body == nil {
		return out, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, err
	}
	if len(raw) == 0 || raw[0] != '{' {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return names{}, err
	}
	return out, nil
}
