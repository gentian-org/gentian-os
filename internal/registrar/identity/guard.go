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

// Who administers the platform is not the registrar's to change.
//
// The platform's administrators are the members of one Keycloak group, named
// on the Cluster claim (spec.platformRoles.admin); the operator projects its
// membership into the authorization store as the cluster's admin relation.
// Whoever can change that group, or take over an account in it, decides who
// administers the cluster. The registrar manages people for whoever the
// store allows, and that is a far wider set of callers than the people who
// may decide that -- so it refuses to be the way it is done, whoever asks.
//
// The rule is applied in ONE place: guard, which doAt calls before every
// request that is not a read. Every call this package makes to Keycloak
// leaves through doAt, so a method added later is covered without knowing
// about the rule, and a write whose shape guard does not recognise is refused
// rather than let through.
//
// What is refused:
//
//   - adding somebody to the group or removing them from it, including
//     creating a person already in it;
//   - renaming or deleting the group, creating a group of that name, renaming
//     another group to it, or creating anything beneath it;
//   - any write to a person who is in the group: their names, address and
//     whether they may sign in, removing them, their second factor, a mailed
//     password link, an activation link. Changing an administrator's address
//     and then mailing a reset is the same as replacing them.
//
// A member's membership of OTHER groups may still be changed: that does not
// touch who administers the platform.
//
// This is a rule in the registrar's code, not a property of its credential.
// The Keycloak client it authenticates as holds manage-users in the realm,
// and Keycloak would carry out any of the above for it.

// ErrProtected is a write that would change who administers the platform.
var ErrProtected = errors.New("the platform administrators are not managed here")

// ErrGuardUnavailable means the rule could not be applied: the group's name
// could not be read. The write is refused, because letting it through would
// be deciding without knowing.
var ErrGuardUnavailable = errors.New("the platform administrators' group could not be determined")

// ErrUnguarded is a write whose shape guard has no rule for. It is a mistake
// in this package and never the caller's, and it is refused.
var ErrUnguarded = errors.New("a write the registrar has no rule for")

// AdminGroup answers the name of the group whose members administer the
// platform, as the Cluster claim names it. Asked on every write rather than
// read once: the claim can change, and the rule has to follow it. More than
// one name is possible only where more than one claim exists; each is
// protected.
type AdminGroup func(ctx context.Context) ([]string, error)

// change is what one request to Keycloak would change, as far as the rule
// cares.
type change struct {
	// user is the person written to, when the write is about one person.
	user string
	// group is the group written to or joined, by Keycloak's id.
	group string
	// membership marks a write that only puts user in group or takes them
	// out. It is judged by the group alone.
	membership bool
}

// classify reads a request path as a write. ok is false for a path this
// package was never meant to write to.
func classify(realm, rel string) (w change, ok bool) {
	admin := "/admin/realms/" + url.PathEscape(realm)
	activation := "/realms/" + url.PathEscape(realm) + "/gentian-activation/users/"
	switch {
	case rel == admin:
		// The realm's own settings: the password policy.
		return change{}, true
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
		case len(seg) == 4 && seg[2] == "groups":
			return change{user: unescape(seg[1]), group: unescape(seg[3]), membership: true}, true
		default:
			return change{user: unescape(seg[1])}, true
		}
	case "groups":
		if len(seg) == 1 {
			return change{}, true
		}
		return change{group: unescape(seg[1])}, true
	}
	return change{}, false
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// guard refuses a write that would change who administers the platform.
//
// Called by doAt for every request that is not a GET, and by nothing else.
func (c *Client) guard(ctx context.Context, r Realm, method, rel string, body any) error {
	w, ok := classify(r.name, rel)
	if !ok {
		return fmt.Errorf("%w: %s %s", ErrUnguarded, method, rel)
	}
	which, err := c.administrators(ctx)
	if err != nil {
		return err
	}

	// What the body itself names: a new group's name, a group's new name,
	// the groups a new person is created in.
	named, err := namedIn(body)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", ErrUnguarded, method, rel, err)
	}
	if name, is := which(named.Name); is && w.user == "" {
		return fmt.Errorf("%w: a group may not be given the name %q", ErrProtected, name)
	}
	for _, g := range named.Groups {
		if name, is := which(g); is {
			return fmt.Errorf("%w: nobody is added to %q from here", ErrProtected, name)
		}
	}

	if w.group != "" {
		var g groupRep
		if err := c.call(ctx, r, http.MethodGet, "/groups/"+url.PathEscape(w.group), nil, nil, &g); err != nil {
			return err
		}
		path := g.Path
		if path == "" {
			path = g.Name
		}
		if name, is := which(path); is {
			if w.membership {
				return fmt.Errorf("%w: membership of %q is not changed from here", ErrProtected, name)
			}
			return fmt.Errorf("%w: the group %q is not changed from here", ErrProtected, name)
		}
	}
	if w.user != "" && !w.membership {
		var groups []groupRep
		if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(w.user)+"/groups", nil, nil, &groups); err != nil {
			return err
		}
		for _, g := range groups {
			if name, is := which(g.Path); is {
				return fmt.Errorf("%w: this person is in %q", ErrProtected, name)
			}
		}
	}
	return nil
}

// administrators asks which groups are the platform administrators' and
// returns the test for them: the administrators' group a path is, or is
// beneath. It fails closed: no name is ErrGuardUnavailable, never "none".
func (c *Client) administrators(ctx context.Context) (func(path string) (string, bool), error) {
	if c.admins == nil {
		return nil, ErrGuardUnavailable
	}
	answered, err := c.admins(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGuardUnavailable, err)
	}
	var admins []string
	for _, name := range answered {
		if name = strings.TrimPrefix(strings.TrimSpace(name), "/"); name != "" {
			admins = append(admins, name)
		}
	}
	if len(admins) == 0 {
		return nil, ErrGuardUnavailable
	}
	return func(path string) (string, bool) {
		path = strings.TrimPrefix(path, "/")
		for _, name := range admins {
			if path == name || strings.HasPrefix(path, name+"/") {
				return name, true
			}
		}
		return "", false
	}, nil
}

// notCustom is the answer for a group the platform composes, which is not
// renamed or deleted from here. For the administrators' group it is
// ErrProtected instead: that group is refused for the stronger reason, and a
// caller told only "not a custom group" would not learn that nothing here
// will ever change it. This decides which refusal is heard and nothing else
// -- the request is refused either way, and guard is what stops the write.
func (c *Client) notCustom(ctx context.Context, path string) error {
	which, err := c.administrators(ctx)
	if err != nil {
		return err
	}
	if name, is := which(path); is {
		return fmt.Errorf("%w: the group %q is not changed from here", ErrProtected, name)
	}
	return fmt.Errorf("%w: %q", ErrNotCustom, path)
}

// names is the part of a request body the rule reads.
type names struct {
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
}

// namedIn reads a body's group name and group list. A body that is not an
// object -- the list of actions a mail carries -- names neither.
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
