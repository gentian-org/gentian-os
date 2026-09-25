/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package identity

import (
	"context"
	"errors"
	"fmt"
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
	Name    string `json:"name,omitempty"`
	Enabled bool   `json:"enabled"`
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
}

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
	q := url.Values{"max": {strconv.Itoa(maxPeople)}}
	if err := c.call(ctx, r, http.MethodGet, "/groups", q, nil, &raw); err != nil {
		return nil, err
	}
	out := []Group{}
	for _, g := range raw {
		path := strings.TrimPrefix(g.Path, "/")
		if !r.permits(path) {
			continue
		}
		out = append(out, Group{ID: g.ID, Path: path, Name: g.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Invitation is what the caller asked for.
type Invitation struct {
	// Email is the address. It is also the username: one identifier for a
	// person, so an invitation and a sign-in cannot disagree about who they
	// are.
	Email string
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
		"username":      email,
		"email":         email,
		"enabled":       true,
		"emailVerified": false,
		"groups":        groupPaths(groups),
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
	if err := c.call(ctx, r, http.MethodPut,
		"/users/"+url.PathEscape(id)+"/execute-actions-email", q, actions, nil); err != nil {
		// The person exists and the mail did not go. Say exactly that: the
		// repair is to re-send, not to invite again, and an error that hid
		// the creation would send somebody to the wrong one.
		return Person{ID: id, Username: email, Email: email, Enabled: true, Pending: true},
			fmt.Errorf("invited %s but the mail was not sent: %w", email, err)
	}
	return Person{ID: id, Username: email, Email: email, Enabled: true, Pending: true,
		Groups: groupPaths(groups)}, nil
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
// director happens to have read a setting it now asserts -- and a field it
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
		ID:       u.ID,
		Username: u.Username,
		Email:    u.Email,
		Name:     name,
		Enabled:  u.Enabled,
		Pending:  !u.EmailVerified || len(u.RequiredActions) > 0,
	}
}

type groupRep struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
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
