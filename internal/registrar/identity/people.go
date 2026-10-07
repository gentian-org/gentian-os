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
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// The three things a tenant administrator has to be able to do, and the reads
// the screens that offer them need. Nothing here decides anything: each one is
// reached only from a route that has already asked OpenFGA about the caller.

// ErrOutOfScope is a group this realm token may not touch. A tenant that has
// its own realm is confined by the credential; a tenant that adopts the
// kernel realm shares it with every other administrator, and there the
// confinement has to be the group subtree.
var ErrOutOfScope = errors.New("group is outside this scope")

// maxPeople bounds one answer. A realm's whole membership is not what a
// screen opens with, and a search is how somebody finds one person.
const maxPeople = 200

// Person is somebody in a realm, as the console shows them.
type Person struct {
	ID string `json:"id"`
	// Username is Keycloak's login name. The platform creates it from the
	// address, so for anybody invited through here the two are the same.
	Username string `json:"username"`
	Email    string `json:"email"`
	// Name is the display name, empty until the person sets it.
	Name string `json:"name,omitempty"`
	// FirstName and LastName are the two halves, for a form that edits them.
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
	Enabled   bool   `json:"enabled"`
	// TOTPConfigured reports an authenticator the person has enrolled;
	// TOTPRequired one they must enrol at their next sign-in. Filled only by
	// the call that reads one person, like Groups.
	TOTPConfigured bool `json:"totpConfigured"`
	TOTPRequired   bool `json:"totpRequired"`
	// Pending reports somebody who has been invited and has not finished:
	// the address is unverified or a required action is outstanding.
	Pending bool `json:"pending"`
	// Groups are the group paths this person holds, without the leading '/'.
	// Filled only by the call that asks for them; listing a realm does not
	// read every person's groups.
	Groups []string `json:"groups,omitempty"`
}

// Group is one group in a realm.
type Group struct {
	ID string `json:"id"`
	// Path is Keycloak's path without the leading '/'. This is the name the
	// authorization graph uses, so it is what the console and the record
	// should say too.
	Path string `json:"path"`
	Name string `json:"name"`
	// Custom marks a group an administrator made, as opposed to one the
	// platform composes (a tenant's admin group, an app's entitlement).
	// Only a custom group can be deleted from here.
	Custom bool `json:"custom"`
	// DefaultGrant marks an app's group whose app the tenant provisioned for
	// everybody rather than only installed: the console ticks it when a
	// person is added, so such an app is opt-out for new people and an
	// installed one is opt-in. It decides nothing about who may enter.
	DefaultGrant bool `json:"defaultGrant,omitempty"`
}

// DefaultGrantAttribute is the Keycloak group attribute the operator sets
// when an app is provisioned for a tenant's people (keycloak.DefaultGrantAttribute).
const DefaultGrantAttribute = "gentianDefaultGrant"

// CustomGroupAttribute marks a group created through CreateGroup.
const CustomGroupAttribute = "gentian.custom"

// configureTOTP is Keycloak's required action for enrolling an authenticator.
const configureTOTP = "CONFIGURE_TOTP"

// inviteEmailAttribute mirrors the delivery address on the person, for
// whoever reads Keycloak's own console; the email field is what Keycloak
// actually mails.
const inviteEmailAttribute = "gentian.inviteEmail"

// Scoped confines a realm token to one group subtree.
//
// A tenant with its own realm needs no scope: the credential is the boundary.
// A tenant that adopts the kernel realm shares it with the platform's own
// administrators and with any other tenant that adopted it, and there the
// only boundary left is the group prefix the platform assigns -- so it is
// stated rather than assumed.
func (r Realm) Scoped(prefix string) Realm {
	r.groupPrefix = prefix
	return r
}

// permits reports whether a group path is inside this realm token's scope.
func (r Realm) permits(path string) bool {
	if r.groupPrefix == "" {
		return true
	}
	return strings.HasPrefix(strings.TrimPrefix(path, "/"), r.groupPrefix)
}

// People lists a realm's people, optionally filtered by a search string that
// Keycloak matches against username, address and name.
func (c *Client) People(ctx context.Context, r Realm, search string, limit int) ([]Person, error) {
	if limit <= 0 || limit > maxPeople {
		limit = maxPeople
	}
	q := url.Values{
		"max":                 {strconv.Itoa(limit)},
		"briefRepresentation": {"true"},
	}
	if search != "" {
		q.Set("search", search)
	}
	var raw []userRep
	if err := c.call(ctx, r, http.MethodGet, "/users", q, nil, &raw); err != nil {
		return nil, err
	}
	people := make([]Person, 0, len(raw))
	for _, u := range raw {
		people = append(people, u.person())
	}
	sort.Slice(people, func(i, j int) bool { return people[i].Username < people[j].Username })
	return people, nil
}

// UserCount answers how many enabled accounts a realm holds, as Keycloak
// counts them. It is a number and nothing about anybody: no name leaves the
// realm to produce it.
func (c *Client) UserCount(ctx context.Context, r Realm) (int, error) {
	var n int
	if err := c.call(ctx, r, http.MethodGet, "/users/count", url.Values{"enabled": {"true"}}, nil, &n); err != nil {
		return 0, err
	}
	return n, nil
}

// Person reads one person, with the groups they hold.
func (c *Client) Person(ctx context.Context, r Realm, id string) (Person, error) {
	if !plainID(id) {
		return Person{}, fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	var u userRep
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(id), nil, nil, &u); err != nil {
		return Person{}, err
	}
	p := u.person()
	p.TOTPRequired = containsString(u.RequiredActions, configureTOTP)
	var creds []struct {
		Type string `json:"type"`
	}
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(id)+"/credentials", nil, nil, &creds); err != nil {
		return Person{}, err
	}
	for _, cr := range creds {
		if cr.Type == "otp" {
			p.TOTPConfigured = true
		}
	}
	var groups []groupRep
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(id)+"/groups", nil, nil, &groups); err != nil {
		return Person{}, err
	}
	for _, g := range groups {
		path := strings.TrimPrefix(g.Path, "/")
		// A person in a shared realm may hold groups from outside this
		// scope. Reporting them here would leak another tenant's membership
		// into this tenant's screen.
		if !r.permits(path) {
			continue
		}
		p.Groups = append(p.Groups, path)
	}
	sort.Strings(p.Groups)
	return p, nil
}

// Groups lists the groups this realm token may administer.
func (c *Client) Groups(ctx context.Context, r Realm) ([]Group, error) {
	var raw []groupRep
	// The full representation, for the attribute that marks a custom group.
	q := url.Values{"max": {strconv.Itoa(maxPeople)}, "briefRepresentation": {"false"}}
	if err := c.call(ctx, r, http.MethodGet, "/groups", q, nil, &raw); err != nil {
		return nil, err
	}
	out := []Group{}
	for _, g := range raw {
		path := strings.TrimPrefix(g.Path, "/")
		if !r.permits(path) {
			continue
		}
		custom := len(g.Attributes[CustomGroupAttribute]) > 0 && g.Attributes[CustomGroupAttribute][0] == "true"
		defaultGrant := len(g.Attributes[DefaultGrantAttribute]) > 0 && g.Attributes[DefaultGrantAttribute][0] == "true"
		out = append(out, Group{ID: g.ID, Path: path, Name: g.Name, Custom: custom, DefaultGrant: defaultGrant})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ZoneLanding is where the realm's own zone client says a person should land.
//
// Read off the client rather than derived. The zone's domain is a custom one,
// or the kernel domain for the user tenant of a single-tenancy cluster, or the
// tenant's subdomain of it,
// and the composition already resolved which -- a second derivation here would
// be a second answer, and the one that disagreed would produce a redirect
// Keycloak refuses with "Invalid parameter: redirect_uri" on a page that says
// nothing about a list.
//
// Empty when the client has no rootUrl, which is what a realm composed before
// this looks like. An invitation then carries no redirect, which is the old
// behaviour rather than a failure.
func (c *Client) ZoneLanding(ctx context.Context, r Realm, clientID string) string {
	var found []struct {
		ClientID string `json:"clientId"`
		RootURL  string `json:"rootUrl"`
		BaseURL  string `json:"baseUrl"`
	}
	q := url.Values{"clientId": {clientID}}
	if err := c.call(ctx, r, http.MethodGet, "/clients", q, nil, &found); err != nil {
		return ""
	}
	for _, f := range found {
		if f.ClientID != clientID {
			continue
		}
		base := f.RootURL
		if base == "" {
			base = f.BaseURL
		}
		if base == "" {
			return ""
		}
		// The list carries the root with its trailing slash; rootUrl is
		// conventionally written without one, and a redirect that differs from
		// the permitted URI by that slash is refused.
		return strings.TrimSuffix(base, "/") + "/"
	}
	return ""
}

// Invitation is what the caller asked for.
type Invitation struct {
	// Email is where the invitation goes, and later a password reset: the
	// address Keycloak mails. When Username is empty it is also the login.
	Email string
	// Username is the login, <local>@<tenant domain>, when it is not the
	// mailing address -- a workspace address the person does not yet read
	// mail at. The caller composes it; this package only checks its shape.
	Username string
	// FirstName and LastName are what the realm greets them with.
	FirstName, LastName string
	// RequireTOTP makes enrolling an authenticator part of accepting.
	RequireTOTP bool
	// Groups are the group paths to put them in, each of which must be
	// inside the realm token's scope.
	Groups []string
	// RedirectURI is where the link lands once they have set a password, and
	// ClientID is the client that link is for -- Keycloak refuses an
	// action-token email that names neither.
	RedirectURI string
	ClientID    string
}

// Invite creates a person and sends them the link that lets them set a
// password.
//
// Created disabled-until-verified rather than with a password: the platform
// never holds one, and an invitation that carried a password would have to be
// transported somewhere. Keycloak's action token does the same job with a
// link that expires.
//
// Not idempotent, deliberately. A second invitation for an address that
// already exists is ErrConflict rather than a silent re-send, because the two
// have different answers: re-sending is a separate act against a person who
// is already there, and quietly doing it would make "invite" mean two things.
func (c *Client) Invite(ctx context.Context, r Realm, inv Invitation) (Person, error) {
	email := strings.TrimSpace(strings.ToLower(inv.Email))
	if !looksLikeAddress(email) {
		return Person{}, fmt.Errorf("not an address: %q", inv.Email)
	}
	username := strings.TrimSpace(strings.ToLower(inv.Username))
	if username == "" {
		username = email
	}
	if !looksLikeAddress(username) {
		return Person{}, fmt.Errorf("not an address: %q", inv.Username)
	}
	groups, err := c.resolveGroups(ctx, r, inv.Groups)
	if err != nil {
		return Person{}, err
	}

	// The group paths go in the creation itself. Creating the person and then
	// adding them leaves a window in which somebody exists in the realm and
	// belongs to nothing, and a failure in the second call would leave them
	// there permanently -- signed in, entitled to nothing, and invisible on
	// the screen that lists a group's members.
	body := map[string]any{
		"username":      username,
		"email":         email,
		"firstName":     strings.TrimSpace(inv.FirstName),
		"lastName":      strings.TrimSpace(inv.LastName),
		"enabled":       true,
		"emailVerified": false,
		"groups":        groupPaths(groups),
	}
	if username != email {
		body["attributes"] = map[string][]string{inviteEmailAttribute: {email}}
	}
	resp, err := c.do(ctx, r, http.MethodPost, "/users", nil, body)
	if err != nil {
		return Person{}, err
	}
	location := resp.Header.Get("Location")
	_ = resp.Body.Close()
	if err := statusError(resp.StatusCode, http.MethodPost, "/users", nil); err != nil {
		return Person{}, err
	}
	id := location[strings.LastIndex(location, "/")+1:]
	if id == "" || !plainID(id) {
		return Person{}, fmt.Errorf("keycloak created a user and named no id")
	}

	// VERIFY_EMAIL proves the address reaches them, UPDATE_PASSWORD is what
	// they came to do. Both in one link, so an invitation is one mail.
	q := url.Values{}
	if inv.ClientID != "" {
		q.Set("client_id", inv.ClientID)
	}
	if inv.RedirectURI != "" {
		q.Set("redirect_uri", inv.RedirectURI)
	}
	actions := []string{"VERIFY_EMAIL", "UPDATE_PASSWORD"}
	if inv.RequireTOTP {
		actions = append(actions, configureTOTP)
	}
	if err := c.call(ctx, r, http.MethodPut,
		"/users/"+url.PathEscape(id)+"/execute-actions-email", q, actions, nil); err != nil {
		// The person exists and the mail did not go. Say exactly that: the
		// repair is to re-send, not to invite again, and an error that hid
		// the creation would send somebody to the wrong one.
		return Person{ID: id, Username: username, Email: email, Enabled: true, Pending: true},
			fmt.Errorf("invited %s but the mail was not sent: %w", username, err)
	}
	return Person{ID: id, Username: username, Email: email, Enabled: true, Pending: true,
		FirstName: strings.TrimSpace(inv.FirstName), LastName: strings.TrimSpace(inv.LastName),
		TOTPRequired: inv.RequireTOTP, Groups: groupPaths(groups)}, nil
}

// SendPasswordReset mails somebody a link that lets them set a new password.
//
// The administrator-initiated half, and the only half that belongs here. A
// person who has locked themselves out has no token, so there is no caller for
// OpenFGA to answer about and an endpoint for them would be an unauthenticated
// write into Keycloak -- which is the invariant this whole package exists to
// keep. Self-service reset is Keycloak's own login page, which sends the same
// mail and holds no credential of ours.
//
// What needs us is the case where an address no longer reaches them and an
// administrator has to act: that has a caller, it is checked, and it is
// recorded.
//
// The same action token as an invitation, with UPDATE_PASSWORD alone: the
// address was verified when they joined, and asking them to verify it again
// would be a second thing to explain.
func (c *Client) SendPasswordReset(ctx context.Context, r Realm, userID, clientID, redirectURI string) error {
	if !plainID(userID) {
		return fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	q := url.Values{}
	if clientID != "" {
		q.Set("client_id", clientID)
	}
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	return c.call(ctx, r, http.MethodPut,
		"/users/"+url.PathEscape(userID)+"/execute-actions-email", q,
		[]string{"UPDATE_PASSWORD"}, nil)
}

// SetMembership adds or removes one person from one group.
//
// The group is named by path rather than by id, because the path is what the
// authorization graph, the console and the record all call it, and an id is
// what only Keycloak calls it.
func (c *Client) SetMembership(ctx context.Context, r Realm, userID, groupPath string, member bool) error {
	if !plainID(userID) {
		return fmt.Errorf("%w: user %q", ErrNotFound, userID)
	}
	groups, err := c.resolveGroups(ctx, r, []string{groupPath})
	if err != nil {
		return err
	}
	path := "/users/" + url.PathEscape(userID) + "/groups/" + url.PathEscape(groups[0].ID)
	method := http.MethodDelete
	if member {
		method = http.MethodPut
	}
	return c.call(ctx, r, method, path, nil, nil, nil)
}

// PersonUpdate names what to change; a nil field is left as it is.
type PersonUpdate struct {
	FirstName *string
	LastName  *string
	Enabled   *bool
	// Email is the delivery address: where a reset or a two-factor link goes.
	Email *string
}

// UpdatePerson changes a person's names, delivery address or whether they
// may sign in.
//
// Read-modify-write of the user, because Keycloak's user update replaces the
// attributes it is sent: a partial body would drop the ones it did not name.
func (c *Client) UpdatePerson(ctx context.Context, r Realm, id string, u PersonUpdate) (Person, error) {
	if !plainID(id) {
		return Person{}, fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	var cur map[string]any
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(id), nil, nil, &cur); err != nil {
		return Person{}, err
	}
	if cur == nil {
		return Person{}, fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	if u.FirstName != nil {
		cur["firstName"] = strings.TrimSpace(*u.FirstName)
	}
	if u.LastName != nil {
		cur["lastName"] = strings.TrimSpace(*u.LastName)
	}
	if u.Enabled != nil {
		cur["enabled"] = *u.Enabled
	}
	if u.Email != nil {
		email := strings.TrimSpace(strings.ToLower(*u.Email))
		if !looksLikeAddress(email) {
			return Person{}, fmt.Errorf("not an address: %q", *u.Email)
		}
		cur["email"] = email
		attrs, _ := cur["attributes"].(map[string]any)
		if attrs == nil {
			attrs = map[string]any{}
		}
		if username, _ := cur["username"].(string); username != email {
			attrs[inviteEmailAttribute] = []string{email}
		} else {
			delete(attrs, inviteEmailAttribute)
		}
		cur["attributes"] = attrs
	}
	if err := c.call(ctx, r, http.MethodPut, "/users/"+url.PathEscape(id), nil, cur, nil); err != nil {
		return Person{}, err
	}
	return c.Person(ctx, r, id)
}

// RemovePerson deletes somebody from the realm.
func (c *Client) RemovePerson(ctx context.Context, r Realm, id string) error {
	if !plainID(id) {
		return fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	return c.call(ctx, r, http.MethodDelete, "/users/"+url.PathEscape(id), nil, nil, nil)
}

// RequireTOTP makes a person enrol an authenticator at their next sign-in,
// and when mail is asked for, sends them the link to do it now.
func (c *Client) RequireTOTP(ctx context.Context, r Realm, id string, mail bool, clientID, redirectURI string) error {
	if !plainID(id) {
		return fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	if err := c.setRequiredAction(ctx, r, id, configureTOTP, true); err != nil {
		return err
	}
	if !mail {
		return nil
	}
	q := url.Values{}
	if clientID != "" {
		q.Set("client_id", clientID)
	}
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	return c.call(ctx, r, http.MethodPut,
		"/users/"+url.PathEscape(id)+"/execute-actions-email", q, []string{configureTOTP}, nil)
}

// RemoveTOTP deletes every authenticator a person enrolled and drops the
// requirement, for somebody who lost their device.
func (c *Client) RemoveTOTP(ctx context.Context, r Realm, id string) error {
	if !plainID(id) {
		return fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	var creds []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(id)+"/credentials", nil, nil, &creds); err != nil {
		return err
	}
	for _, cr := range creds {
		if cr.Type != "otp" || !plainID(cr.ID) {
			continue
		}
		if err := c.call(ctx, r, http.MethodDelete,
			"/users/"+url.PathEscape(id)+"/credentials/"+url.PathEscape(cr.ID), nil, nil, nil); err != nil {
			return err
		}
	}
	return c.setRequiredAction(ctx, r, id, configureTOTP, false)
}

// setRequiredAction adds or removes one required action, keeping the others.
func (c *Client) setRequiredAction(ctx context.Context, r Realm, id, action string, on bool) error {
	var cur map[string]any
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(id), nil, nil, &cur); err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	var actions []string
	if raw, ok := cur["requiredActions"].([]any); ok {
		for _, a := range raw {
			if s, ok := a.(string); ok && s != action {
				actions = append(actions, s)
			}
		}
	}
	if on {
		actions = append(actions, action)
	}
	if actions == nil {
		actions = []string{}
	}
	cur["requiredActions"] = actions
	return c.call(ctx, r, http.MethodPut, "/users/"+url.PathEscape(id), nil, cur, nil)
}

// CreateGroup makes a custom group at the given path. The caller composes
// the path inside its tenant's subtree; it must be inside this token's scope.
func (c *Client) CreateGroup(ctx context.Context, r Realm, path string) (Group, error) {
	path = strings.TrimPrefix(path, "/")
	if path == "" || strings.Contains(path, "/") {
		return Group{}, fmt.Errorf("%w: group %q", ErrNotFound, path)
	}
	if !r.permits(path) {
		return Group{}, fmt.Errorf("%w: %q", ErrOutOfScope, path)
	}
	body := map[string]any{
		"name":       path,
		"attributes": map[string][]string{CustomGroupAttribute: {"true"}},
	}
	if err := c.call(ctx, r, http.MethodPost, "/groups", nil, body, nil); err != nil {
		return Group{}, err
	}
	groups, err := c.resolveGroups(ctx, r, []string{path})
	if err != nil {
		return Group{}, err
	}
	return groups[0], nil
}

// ErrNotCustom is a group the platform composes, which is not deleted from here.
var ErrNotCustom = errors.New("group is the platform's, not a custom one")

// DeleteGroup removes a custom group. A group the platform composed -- an
// app's entitlement, the tenant's admins -- is refused: its owner would only
// make it again, and everybody in it would lose access until it did.
func (c *Client) DeleteGroup(ctx context.Context, r Realm, path string) error {
	groups, err := c.resolveGroups(ctx, r, []string{path})
	if err != nil {
		return err
	}
	if !groups[0].Custom {
		return c.notCustom(ctx, groups[0].Path)
	}
	return c.call(ctx, r, http.MethodDelete, "/groups/"+url.PathEscape(groups[0].ID), nil, nil, nil)
}

// Activation is how an account was handed over: mailed to an address, or a
// link to show the person who asked, once.
type Activation struct {
	Mailed bool   `json:"mailed"`
	Email  string `json:"email,omitempty"`
	Link   string `json:"link,omitempty"`
	// ExpiresAt is the link's expiry, seconds since the epoch.
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// Actions are what the link asks the person to do.
	Actions []string `json:"actions"`
	// MailError says why a link meant for Email is shown instead of mailed.
	MailError string `json:"mailError,omitempty"`
}

// FindUser finds one person by exact username.
func (c *Client) FindUser(ctx context.Context, r Realm, username string) (Person, error) {
	var raw []userRep
	q := url.Values{"username": {username}, "exact": {"true"}, "briefRepresentation": {"true"}}
	if err := c.call(ctx, r, http.MethodGet, "/users", q, nil, &raw); err != nil {
		return Person{}, err
	}
	for _, u := range raw {
		if strings.EqualFold(u.Username, username) {
			return u.person(), nil
		}
	}
	return Person{}, fmt.Errorf("%w: user %q", ErrNotFound, username)
}

// ActivateAccount hands an account to its holder through a single-use,
// expiring link: set a password, and enrol a second factor when one is
// required and not yet enrolled.
//
// The platform's practice for every person it invites, now for administrators
// too. Mailed when there is an address to mail -- the one given now, which
// becomes the account's recovery address, or the one it already has -- and
// otherwise returned, for whoever asked to show once. Either way nobody but
// the holder ever knows the password. Issuing it again is how an
// administrator who lost access gets it back: a new link, not a lookup.
func (c *Client) ActivateAccount(ctx context.Context, r Realm, id, email string, requireMFA bool, clientID, redirectURI string) (Activation, error) {
	if !plainID(id) {
		return Activation{}, fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	var creds []struct {
		Type string `json:"type"`
	}
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(id)+"/credentials", nil, nil, &creds); err != nil {
		return Activation{}, err
	}
	hasOTP := false
	for _, cr := range creds {
		hasOTP = hasOTP || cr.Type == "otp"
	}
	actions := []string{"UPDATE_PASSWORD"}
	if requireMFA && !hasOTP {
		actions = append(actions, configureTOTP)
	}

	var cur map[string]any
	if err := c.call(ctx, r, http.MethodGet, "/users/"+url.PathEscape(id), nil, nil, &cur); err != nil {
		return Activation{}, err
	}
	if cur == nil {
		return Activation{}, fmt.Errorf("%w: user %q", ErrNotFound, id)
	}
	email = strings.TrimSpace(strings.ToLower(email))
	if email != "" {
		if !looksLikeAddress(email) {
			return Activation{}, fmt.Errorf("not an address: %q", email)
		}
		cur["email"] = email
		attrs, _ := cur["attributes"].(map[string]any)
		if attrs == nil {
			attrs = map[string]any{}
		}
		attrs[inviteEmailAttribute] = []string{email}
		cur["attributes"] = attrs
	}
	// The steps stay required whatever happens to the link, so a session
	// opened some other way cannot skip them.
	required := map[string]bool{}
	if raw, ok := cur["requiredActions"].([]any); ok {
		for _, a := range raw {
			if s, ok := a.(string); ok {
				required[s] = true
			}
		}
	}
	for _, a := range actions {
		required[a] = true
	}
	list := make([]string, 0, len(required))
	for a := range required {
		list = append(list, a)
	}
	sort.Strings(list)
	cur["requiredActions"] = list
	if err := c.call(ctx, r, http.MethodPut, "/users/"+url.PathEscape(id), nil, cur, nil); err != nil {
		return Activation{}, err
	}

	mailTo, _ := cur["email"].(string)
	q := url.Values{}
	if clientID != "" {
		q.Set("client_id", clientID)
	}
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	// Mailed when the realm can send mail; shown otherwise. With an external
	// relay the credential arrives through the console, so a fresh cluster --
	// a tunnel one always -- has no mail server yet, and refusing the
	// activation over that would leave the administrator with no way in. The
	// address stays on the account for later resets either way.
	mailError := ""
	if mailTo != "" {
		err := c.call(ctx, r, http.MethodPut, "/users/"+url.PathEscape(id)+"/execute-actions-email", q, actions, nil)
		if err == nil {
			return Activation{Mailed: true, Email: mailTo, Actions: actions}, nil
		}
		mailError = "the realm could not send mail (is the SMTP relay configured?); the link is shown instead"
	}

	body := map[string]any{"actions": actions}
	if clientID != "" {
		body["clientId"] = clientID
	}
	if redirectURI != "" {
		body["redirectUri"] = redirectURI
	}
	rel := "/realms/" + url.PathEscape(r.name) + "/gentian-activation/users/" + url.PathEscape(id) + "/link"
	resp, err := c.doAt(ctx, r, http.MethodPost, rel, "/gentian-activation/users/{id}/link", nil, body)
	if err != nil {
		return Activation{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := statusError(resp.StatusCode, http.MethodPost, "/gentian-activation/users/{id}/link", raw); err != nil {
		return Activation{}, err
	}
	var out struct {
		Link      string `json:"link"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Link == "" {
		return Activation{}, fmt.Errorf("the realm answered no activation link")
	}
	return Activation{Link: out.Link, ExpiresAt: out.ExpiresAt, Actions: actions, Email: mailTo, MailError: mailError}, nil
}

// RenameGroup gives a custom group a new path. The platform's own groups keep
// their names: other things find them by name.
func (c *Client) RenameGroup(ctx context.Context, r Realm, path, newPath string) (Group, error) {
	newPath = strings.TrimPrefix(newPath, "/")
	if newPath == "" || strings.Contains(newPath, "/") {
		return Group{}, fmt.Errorf("%w: group %q", ErrNotFound, newPath)
	}
	if !r.permits(newPath) {
		return Group{}, fmt.Errorf("%w: %q", ErrOutOfScope, newPath)
	}
	groups, err := c.resolveGroups(ctx, r, []string{path})
	if err != nil {
		return Group{}, err
	}
	if !groups[0].Custom {
		return Group{}, c.notCustom(ctx, groups[0].Path)
	}
	var cur map[string]any
	if err := c.call(ctx, r, http.MethodGet, "/groups/"+url.PathEscape(groups[0].ID), nil, nil, &cur); err != nil {
		return Group{}, err
	}
	if cur == nil {
		cur = map[string]any{}
	}
	cur["name"] = newPath
	if err := c.call(ctx, r, http.MethodPut, "/groups/"+url.PathEscape(groups[0].ID), nil, cur, nil); err != nil {
		return Group{}, err
	}
	return Group{ID: groups[0].ID, Path: newPath, Name: newPath, Custom: true}, nil
}

// GroupMembers lists the people in one group.
func (c *Client) GroupMembers(ctx context.Context, r Realm, path string) ([]Person, error) {
	groups, err := c.resolveGroups(ctx, r, []string{path})
	if err != nil {
		return nil, err
	}
	var raw []userRep
	q := url.Values{"max": {strconv.Itoa(maxPeople)}, "briefRepresentation": {"true"}}
	if err := c.call(ctx, r, http.MethodGet, "/groups/"+url.PathEscape(groups[0].ID)+"/members", q, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Person, 0, len(raw))
	for _, u := range raw {
		out = append(out, u.person())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// PasswordPolicy reads the realm's policy, in Keycloak's own spelling
// ("length(12) and notUsername(undefined)"). Returned as written rather than
// parsed: the console shows it and the platform does not interpret it, and a
// parse would have to be kept in step with a vocabulary Keycloak extends.
func (c *Client) PasswordPolicy(ctx context.Context, r Realm) (string, error) {
	var rep struct {
		PasswordPolicy string `json:"passwordPolicy"`
	}
	if err := c.call(ctx, r, http.MethodGet, "", nil, nil, &rep); err != nil {
		return "", err
	}
	return rep.PasswordPolicy, nil
}

// SetPasswordPolicy writes it.
//
// A partial representation, not a read-modify-write of the whole realm.
// Keycloak applies the fields a representation names and leaves the rest, so
// sending the whole realm back would make every unrelated setting this
// registrar happens to have read a setting it now asserts -- and a field it
// did not understand would be rewritten with whatever it decoded.
func (c *Client) SetPasswordPolicy(ctx context.Context, r Realm, policy string) error {
	body := map[string]any{"realm": r.name, "passwordPolicy": policy}
	return c.call(ctx, r, http.MethodPut, "", nil, body, nil)
}

// resolveGroups turns group paths into groups, refusing any outside scope.
//
// The scope check happens here, once, rather than in each caller: every write
// that names a group goes through this function, so a new one cannot forget.
func (c *Client) resolveGroups(ctx context.Context, r Realm, paths []string) ([]Group, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	known, err := c.Groups(ctx, r)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]Group, len(known))
	for _, g := range known {
		byPath[g.Path] = g
	}
	out := make([]Group, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimPrefix(p, "/")
		if !r.permits(p) {
			return nil, fmt.Errorf("%w: %q", ErrOutOfScope, p)
		}
		g, ok := byPath[p]
		if !ok {
			// Out of scope and not there are different answers, and this is
			// the second: Groups() already dropped anything outside scope,
			// so what is left is a group this realm does not have.
			return nil, fmt.Errorf("%w: group %q", ErrNotFound, p)
		}
		out = append(out, g)
	}
	return out, nil
}

func groupPaths(groups []Group) []string {
	if len(groups) == 0 {
		return nil
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		// Keycloak's user representation wants the path with its leading
		// slash; everything else in this platform says it without.
		out = append(out, "/"+g.Path)
	}
	return out
}

// userRep and groupRep are the parts of Keycloak's representations this reads.
type userRep struct {
	ID              string   `json:"id"`
	Username        string   `json:"username"`
	Email           string   `json:"email"`
	FirstName       string   `json:"firstName"`
	LastName        string   `json:"lastName"`
	Enabled         bool     `json:"enabled"`
	EmailVerified   bool     `json:"emailVerified"`
	RequiredActions []string `json:"requiredActions"`
}

func (u userRep) person() Person {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	return Person{
		ID:        u.ID,
		Username:  u.Username,
		Email:     u.Email,
		Name:      name,
		FirstName: u.FirstName,
		LastName:  u.LastName,
		Enabled:   u.Enabled,
		// Pending is somebody who has not finished accepting: an unverified
		// address, or a password not yet set. A two-factor requirement added
		// later to a person who signs in already is not "pending".
		Pending:      !u.EmailVerified || containsString(u.RequiredActions, "UPDATE_PASSWORD"),
		TOTPRequired: containsString(u.RequiredActions, configureTOTP),
	}
}

type groupRep struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Path       string              `json:"path"`
	Attributes map[string][]string `json:"attributes"`
}

// plainID rejects anything that is not a Keycloak id, before it reaches a
// URL. Keycloak's ids are UUIDs; what matters here is that nothing with a
// path separator or a query character can travel in a path segment.
func plainID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// looksLikeAddress is a shape check and not a validation: whether the address
// exists is settled by the invitation arriving, which is the only test that
// means anything.
func looksLikeAddress(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.Count(s, "@") != 1 {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n/?#") {
		return false
	}
	return strings.Contains(s[at+1:], ".")
}
