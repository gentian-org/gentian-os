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

// Who holds a platform role is not the registrar's to change.
//
// A platform role -- administrator, security officer, auditor, service
// administrator, shared-apps administrator, break-glass, and whatever the
// Cluster claim's platformRoles names besides -- is held by the members of one
// Keycloak group, named on the claim (spec.platformRoles.<role>); the operator
// projects each into the authorization store as a relation on the cluster.
// Whoever can change one of those groups, or take over an account in it,
// decides who holds that role over the cluster. The registrar manages people
// for whoever the store allows, and that is a far wider set of callers than
// the people who may decide that -- so it refuses to be the way it is done,
// whoever asks.
//
// The rule is applied in ONE place: guard, which doAt calls before every
// request that is not a read. Every call this package makes to Keycloak
// leaves through doAt, so a method added later is covered without knowing
// about the rule, and a write whose shape guard does not recognise is refused
// rather than let through.
//
// What is refused, for every one of those groups alike:
//
//   - adding somebody to the group or removing them from it, including
//     creating a person already in it;
//   - renaming or deleting the group, creating a group of that name, renaming
//     another group to it, or creating anything beneath it;
//   - any write to a person who is in the group: their names, address and
//     whether they may sign in, removing them, their second factor, a mailed
//     password link, an activation link. Changing a role holder's address
//     and then mailing a reset is the same as replacing them.
//
// A member's membership of groups that hold no platform role may still be
// changed: that does not touch who holds one.
//
// This is a rule in the registrar's code, not a property of its credential.
// The Keycloak client it authenticates as holds manage-users in the realm,
// and Keycloak would carry out any of the above for it.

// ErrProtected is a write that would change who holds a platform role.
var ErrProtected = errors.New("the holders of the platform's roles are not managed here")

// ErrApproversOnly is a write that would change who approves a tenant's
// public addresses, asked by somebody who may not approve them.
var ErrApproversOnly = errors.New("who approves this tenant's public addresses is changed only by somebody who may approve them")

// Who approves what a tenant publishes to the internet is held by the members
// of one more group, the tenant's own: gentian:tenant:<t>:perimeter. Unlike a
// platform role's group it IS managed here -- but only by a caller who holds
// the right its members hold. Whoever may add a person to that group, or take
// over an account in it, decides who publishes; a tenant's administrator may
// manage people (can_manage_users) without being one of them, and must not be
// able to become one by putting themselves in the group.
//
// So the registrar's routes ask the store whether the caller holds can_expose
// on the tenant, and where they do not, name the group here. guard then
// applies to it everything it applies to a platform role's group: its
// membership, the group itself, and every write to a person in it.

// approversHeldKey is the context key the held-back groups are under.
type approversHeldKey struct{}

// WithApproversHeld marks group as one the caller of ctx may not change the
// holders of.
func WithApproversHeld(ctx context.Context, group string) context.Context {
	group = strings.TrimPrefix(strings.TrimSpace(group), "/")
	if group == "" {
		return ctx
	}
	held, _ := ctx.Value(approversHeldKey{}).([]string)
	return context.WithValue(ctx, approversHeldKey{}, append(append([]string{}, held...), group))
}

// approversHeld is the test for the groups WithApproversHeld named, or nil
// when it named none.
func approversHeld(ctx context.Context) func(path string) (string, bool) {
	held, _ := ctx.Value(approversHeldKey{}).([]string)
	if len(held) == 0 {
		return nil
	}
	return beneath(held)
}

// beneath is the test for a set of group names: the one a path is, or is
// beneath.
func beneath(names []string) func(path string) (string, bool) {
	return func(path string) (string, bool) {
		path = strings.TrimPrefix(path, "/")
		for _, name := range names {
			if path == name || strings.HasPrefix(path, name+"/") {
				return name, true
			}
		}
		return "", false
	}
}

// ErrGuardUnavailable means the rule could not be applied: the groups' names
// could not be read. The write is refused, because letting it through would
// be deciding without knowing.
var ErrGuardUnavailable = errors.New("the platform role groups could not be determined")

// ErrUnguarded is a write whose shape guard has no rule for. It is a mistake
// in this package and never the caller's, and it is refused.
var ErrUnguarded = errors.New("a write the registrar has no rule for")

// RoleGroups answers the names of the groups whose members hold a platform
// role, as the Cluster claim names them: every one, not the administrators'
// alone. Asked on every write rather than read once: the claim can change,
// and the rule has to follow it. Each name answered is protected.
type RoleGroups func(ctx context.Context) ([]string, error)

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

// guard refuses a write that would change who holds a platform role.
//
// Called by doAt for every request that is not a GET, and by nothing else.
func (c *Client) guard(ctx context.Context, r Realm, method, rel string, body any) error {
	w, ok := classify(r.name, rel)
	if !ok {
		return fmt.Errorf("%w: %s %s", ErrUnguarded, method, rel)
	}
	which, err := c.roleGroups(ctx)
	if err != nil {
		return err
	}

	// What the body itself names: a new group's name, a group's new name,
	// the groups a new person is created in.
	named, err := namedIn(body)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", ErrUnguarded, method, rel, err)
	}
	if err := c.refuse(ctx, r, w, named, which, ErrProtected); err != nil {
		return err
	}
	// The same rule, for the groups this caller in particular may not change
	// the holders of (WithApproversHeld).
	if held := approversHeld(ctx); held != nil {
		return c.refuse(ctx, r, w, named, held, ErrApproversOnly)
	}
	return nil
}

// refuse is the rule itself, for one set of groups: which says whether a
// path is one of them, and refusal is what the caller hears when the write
// would change who is in one.
func (c *Client) refuse(
	ctx context.Context, r Realm, w change, named names,
	which func(path string) (string, bool), refusal error,
) error {
	if name, is := which(named.Name); is && w.user == "" {
		return fmt.Errorf("%w: a group may not be given the name %q", refusal, name)
	}
	for _, g := range named.Groups {
		if name, is := which(g); is {
			return fmt.Errorf("%w: nobody is added to %q from here", refusal, name)
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
				return fmt.Errorf("%w: membership of %q is not changed from here", refusal, name)
			}
			return fmt.Errorf("%w: the group %q is not changed from here", refusal, name)
		}
	}
	if w.user != "" && !w.membership {
		var groups []groupRep
		if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(w.user)+"/groups", nil, nil, &groups); err != nil {
			return err
		}
		for _, g := range groups {
			if name, is := which(g.Path); is {
				return fmt.Errorf("%w: this person is in %q", refusal, name)
			}
		}
	}
	return nil
}

// roleGroups asks which groups hold a platform role and returns the test for
// them: the role group a path is, or is beneath. It fails closed: no name is
// ErrGuardUnavailable, never "none".
func (c *Client) roleGroups(ctx context.Context) (func(path string) (string, bool), error) {
	if c.roles == nil {
		return nil, ErrGuardUnavailable
	}
	answered, err := c.roles(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGuardUnavailable, err)
	}
	var protected []string
	for _, name := range answered {
		if name = strings.TrimPrefix(strings.TrimSpace(name), "/"); name != "" {
			protected = append(protected, name)
		}
	}
	if len(protected) == 0 {
		return nil, ErrGuardUnavailable
	}
	return beneath(protected), nil
}

// notCustom is the answer for a group the platform composes, which is not
// renamed or deleted from here. For a platform role's group it is
// ErrProtected instead: that group is refused for the stronger reason, and a
// caller told only "not a custom group" would not learn that nothing here
// will ever change it. This decides which refusal is heard and nothing else
// -- the request is refused either way, and guard is what stops the write.
func (c *Client) notCustom(ctx context.Context, path string) error {
	which, err := c.roleGroups(ctx)
	if err != nil {
		return err
	}
	if name, is := which(path); is {
		return fmt.Errorf("%w: the group %q is not changed from here", ErrProtected, name)
	}
	if held := approversHeld(ctx); held != nil {
		if name, is := held(path); is {
			return fmt.Errorf("%w: the group %q is not changed from here", ErrApproversOnly, name)
		}
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
