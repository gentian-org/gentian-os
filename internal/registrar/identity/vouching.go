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
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

// A component that vouches for people (requires.services.vouching) is entered
// in the tenant's realm as an identity provider, and the realm answers its
// statements only for a person who is linked to that entry. The link is the
// person's consent, as the realm keeps it: this file makes it, reads it and
// takes it away. Who may ask for which of the three is the routes' to decide
// (internal/registrar/vouching.go); nothing here does.
//
// No method takes an alias. Each takes the profile's name and derives the
// alias, so a caller cannot be talked into linking a person to the realm's
// own sign-in providers, or taking such a link away.

// vouchingAliasPrefix is what every such entry's alias starts with.
const vouchingAliasPrefix = "vouch-"

// VouchingAlias is the alias of the identity provider entry a profile's
// component is in the realm under. The operator composes the entry and names
// it the same way (internal/controller.VouchingAlias): the two must agree,
// or a person is linked to an entry nobody made.
func VouchingAlias(profile string) string { return vouchingAliasPrefix + profile }

// profileName is a profile's name: a DNS label, as everywhere on the
// platform. Held to it here because it becomes a path segment.
var profileName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ErrNoSuchIssuer is a profile whose component is not an identity provider of
// the realm: nothing declared it, or the realm has not been told yet.
var ErrNoSuchIssuer = errors.New("no component of that name vouches for people in this realm")

// federatedIdentityRep is one link, in Keycloak's spelling.
type federatedIdentityRep struct {
	IdentityProvider string `json:"identityProvider"`
	UserID           string `json:"userId"`
	UserName         string `json:"userName"`
}

func linkPath(userID, alias string) string {
	return "/users/" + url.PathEscape(userID) + "/federated-identity/" + url.PathEscape(alias)
}

// VouchingLinked answers whether a person is linked to a profile's entry.
//
// Keycloak lists a person's links to the identity providers the realm has
// and leaves out a link to an alias that is not one, so "yes" also says the
// entry exists. ErrNotFound is a person the realm does not have.
func (c *Client) VouchingLinked(ctx context.Context, r Realm, userID, profile string) (bool, error) {
	if !plainID(userID) {
		return false, fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	if !profileName.MatchString(profile) {
		return false, fmt.Errorf("%w: %q", ErrNoSuchIssuer, profile)
	}
	return c.linked(ctx, r, userID, VouchingAlias(profile))
}

func (c *Client) linked(ctx context.Context, r Realm, userID, alias string) (bool, error) {
	var links []federatedIdentityRep
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(userID)+"/federated-identity", nil, nil, &links); err != nil {
		return false, err
	}
	for _, l := range links {
		if l.IdentityProvider == alias {
			return true, nil
		}
	}
	return false, nil
}

// LinkVouching links a person to a profile's entry, so that the component
// may obtain that person's tokens. Linking somebody who is linked changes
// nothing and is not an error.
//
// The link names the person by their own id in the realm, which is what the
// component's statements name them by, and by their username.
//
// It refuses with ErrNoSuchIssuer when the realm has no such entry, and the
// way it learns that is roundabout for a reason. The registrar's roles in
// the realm do not let it read identity providers (Keycloak answers 403),
// and Keycloak makes a link to any alias it is given, whether or not an
// identity provider has it. What Keycloak does do is list only the links
// whose provider exists. So the link is made and then looked for: one that
// is not listed was made to nothing, is taken away again, and is refused.
// It was worth nothing in between, since no entry was there to trust it.
func (c *Client) LinkVouching(ctx context.Context, r Realm, userID, profile string) error {
	if !plainID(userID) {
		return fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	if !profileName.MatchString(profile) {
		return fmt.Errorf("%w: %q", ErrNoSuchIssuer, profile)
	}
	alias := VouchingAlias(profile)
	var u userRep
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(userID), nil, nil, &u); err != nil {
		return err
	}
	if already, err := c.linked(ctx, r, userID, alias); err != nil || already {
		return err
	}
	body := federatedIdentityRep{IdentityProvider: alias, UserID: userID, UserName: u.Username}
	// A conflict is somebody linking the same person at the same moment, and
	// is the state that was asked for.
	if err := c.call(ctx, r, http.MethodPost, linkPath(userID, alias), nil, body, nil); err != nil && !errors.Is(err, ErrConflict) {
		return err
	}
	exists, err := c.linked(ctx, r, userID, alias)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := c.call(ctx, r, http.MethodDelete, linkPath(userID, alias), nil, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
		// Said out loud, because what stays behind would become a real link
		// the day an entry of that alias is made.
		return fmt.Errorf("a link to %q, which is not an identity provider of realm %s, was made for user %s and could not be removed: %w",
			alias, r.name, userID, err)
	}
	return fmt.Errorf("%w: %q", ErrNoSuchIssuer, profile)
}

// UnlinkVouching takes a person's link to a profile's entry away. A person
// who is not linked is left as they are, without an error.
//
// The removal is sent whether or not the link is listed: a link to an alias
// that is not an identity provider is not listed and is still there, and
// would count again once the entry is. ErrNotFound is a person the realm
// does not have.
func (c *Client) UnlinkVouching(ctx context.Context, r Realm, userID, profile string) error {
	if !plainID(userID) {
		return fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	if !profileName.MatchString(profile) {
		// Nothing can be linked under such a name, so nothing is.
		return nil
	}
	alias := VouchingAlias(profile)
	// Read first, for the one thing it tells apart: Keycloak answers 404 for
	// a person it does not have and for a link that is not there alike.
	if _, err := c.linked(ctx, r, userID, alias); err != nil {
		return err
	}
	if err := c.call(ctx, r, http.MethodDelete, linkPath(userID, alias), nil, nil, nil); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}
