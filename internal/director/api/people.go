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

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/identity"
	"github.com/gentian-org/gentian-os/internal/director/record"
)

// People, groups and the realm's password policy — the director speaking for
// Keycloak on a caller's behalf (S7A.17).
//
// The rule is the same as for git and for the authorization graph: a write
// into a source of truth is admissible when it is authenticated, evaluated and
// recorded. People cannot be derived from the state of a cluster and cannot go
// into git, so managing them is an ACTION here rather than a commit — which is
// why every write below is a POST under /actions/ and none is a PUT.
//
// Two things make "indirect" a rule rather than a description:
//
//   - The realm is never taken from the request. It is resolved from the
//     tenant whose object the caller was just checked against, and the
//     identity client refuses a realm it holds no credential for. So a
//     handler cannot be talked into another tenant's realm.
//   - A check that cannot be made is a refusal. The route helpers already do
//     that; nothing here falls back.

// realmFor resolves the realm of the tenant in the request path, and returns a
// realm token only if this director holds a credential for it.
//
// Both failures answer differently on purpose. A tenant with no realm in the
// manifest is a 404 about the tenant. A realm this director was given no
// credential for is a 503 about the platform: the caller may well hold the
// relation, and telling them they are forbidden would send them to the wrong
// person.
func (s *Server) realmFor(w http.ResponseWriter, r *http.Request) (identity.Realm, bool) {
	if s.cfg.Identity == nil {
		s.fail(w, r, http.StatusServiceUnavailable,
			"this director speaks for no realm: it holds no Keycloak credential")
		return identity.Realm{}, false
	}
	tenant := r.PathValue("t")
	realm, err := s.cfg.Repo.TenantRealm(r.Context(), tenant)
	if err != nil {
		s.repoError(w, r, err)
		return identity.Realm{}, false
	}
	token, err := s.cfg.Identity.Realm(realm)
	if err != nil {
		s.cfg.Log.WarnContext(r.Context(), "no Keycloak credential for realm",
			"request_id", reqID(r.Context()), "tenant", tenant, "realm", realm)
		s.fail(w, r, http.StatusServiceUnavailable,
			"this director holds no credential for the realm "+realm+
				": the operator writes one per realm, and this one has not arrived yet")
		return identity.Realm{}, false
	}
	// A tenant that has a realm to itself is confined by the credential. A
	// tenant that shares the kernel realm is not, and there the group subtree
	// is the only boundary left -- so it is stated rather than assumed.
	//
	// Except the platform tenant, which does not share the kernel realm but
	// owns it: its people are the kernel realm's administrators and its groups
	// are gentian:platform:*, so confining it to gentian:tenant:platform:
	// showed it none of its own groups.
	if realm != tenant && tenant != gitops.PlatformTenant {
		token = token.Scoped(tenantGroupPrefix(tenant))
	}
	return token, true
}

// identityContext carries the request id into the admin calls, which is what
// joins Keycloak's record of the change to the director's record of the
// authority.
func identityContext(r *http.Request) context.Context {
	return identity.WithRequestID(r.Context(), reqID(r.Context()))
}

// identityError maps what the realm said onto what the caller should hear.
func (s *Server) identityError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "not found in this tenant's realm")
	case errors.Is(err, identity.ErrConflict):
		s.fail(w, r, http.StatusConflict, "somebody with that address is already here")
	case errors.Is(err, identity.ErrOutOfScope):
		// Not 403. The caller holds the relation; what they named is outside
		// what this tenant may touch, and saying "forbidden" would send them
		// to ask for a permission that would not help.
		s.fail(w, r, http.StatusBadRequest, "that group does not belong to this tenant")
	case errors.Is(err, identity.ErrNoCredential):
		s.fail(w, r, http.StatusServiceUnavailable, "this director holds no credential for that realm")
	default:
		s.cfg.Log.ErrorContext(r.Context(), "keycloak call failed",
			"request_id", reqID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusBadGateway, "the realm could not be reached")
	}
}

// listPeople answers who is in this tenant.
func (s *Server) listPeople(w http.ResponseWriter, r *http.Request, _ call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	people, err := s.cfg.Identity.People(identityContext(r), realm,
		strings.TrimSpace(r.URL.Query().Get("search")), limit)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "people": people})
}

// getPerson answers one person and the groups they hold.
func (s *Server) getPerson(w http.ResponseWriter, r *http.Request, _ call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	person, err := s.cfg.Identity.Person(identityContext(r), realm, r.PathValue("id"))
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, person)
}

// listGroups answers the groups this tenant may put somebody in.
func (s *Server) listGroups(w http.ResponseWriter, r *http.Request, _ call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	groups, err := s.cfg.Identity.Groups(identityContext(r), realm)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "groups": groups})
}

// tenantIdentitySettings answers the realm settings a tenant administrator
// may see. The password policy today; this is the screen the rest of the
// realm's own settings arrive on.
func (s *Server) tenantIdentitySettings(w http.ResponseWriter, r *http.Request, _ call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	policy, err := s.cfg.Identity.PasswordPolicy(identityContext(r), realm)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	// The domain a login is composed under, so the invitation form can show
	// "@<domain>" beside the part a person types. Empty when it cannot be
	// read, and the form then asks for a whole address.
	domain, _ := s.cfg.Repo.TenantLoginDomain(r.Context(), r.PathValue("t"))
	s.json(w, http.StatusOK, map[string]any{
		"tenant":         r.PathValue("t"),
		"realm":          realm.Name(),
		"passwordPolicy": policy,
		"loginDomain":    domain,
		"templates":      s.cfg.DesktopAPI != "",
	})
}

// invitePerson creates somebody and sends them the link that lets them set a
// password.
func (s *Server) invitePerson(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		// Email is where the invitation goes.
		Email string `json:"email"`
		// Username is the part before @<login domain>; empty makes the
		// address the login, as before.
		Username         string   `json:"username"`
		FirstName        string   `json:"firstName"`
		LastName         string   `json:"lastName"`
		RequireTOTP      bool     `json:"requireTotp"`
		SettingsTemplate string   `json:"settingsTemplate"`
		Groups           []string `json:"groups"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	login := ""
	if local := strings.ToLower(strings.TrimSpace(body.Username)); local != "" {
		if !localPart.MatchString(local) {
			s.fail(w, r, http.StatusBadRequest, "a username is letters, digits, dots, dashes and underscores")
			return
		}
		domain, err := s.cfg.Repo.TenantLoginDomain(r.Context(), r.PathValue("t"))
		if err != nil || domain == "" {
			s.fail(w, r, http.StatusServiceUnavailable, "this tenant's login domain cannot be read")
			return
		}
		login = local + "@" + domain
	}
	ctx := identityContext(r)
	client := s.inviteClientID(realm)
	// Where they land, from the zone client itself. A configured value wins,
	// for a deployment that lands people somewhere else on purpose.
	redirect := s.cfg.InviteRedirectURI
	if redirect == "" {
		redirect = s.cfg.Identity.ZoneLanding(ctx, realm, client)
	}
	person, err := s.cfg.Identity.Invite(ctx, realm, identity.Invitation{
		Email:       body.Email,
		Username:    login,
		FirstName:   body.FirstName,
		LastName:    body.LastName,
		RequireTOTP: body.RequireTOTP,
		Groups:      body.Groups,
		ClientID:    client,
		RedirectURI: redirect,
	})
	if err != nil {
		// A person who exists with a mail that did not go is reported as
		// exactly that, because the repair is to re-send rather than to
		// invite again.
		if person.ID != "" {
			s.json(w, http.StatusAccepted, map[string]any{
				"person":  person,
				"mailed":  false,
				"warning": err.Error(),
			})
			return
		}
		if isAddressError(err) {
			s.fail(w, r, http.StatusBadRequest, "that is not an address")
			return
		}
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "invite", realm, person.Username)
	out := map[string]any{"person": person, "mailed": true}
	// The desktop's settings, copied from a template before they first sign
	// in. Not fatal: the person exists and the invitation went, and saying
	// the template did not apply is a repair somebody can make on the
	// template screen, where failing the invitation would not be.
	if body.SettingsTemplate != "" {
		if err := s.applyTemplate(r, body.SettingsTemplate, person.ID); err != nil {
			out["templateApplied"] = false
			out["warning"] = "the settings template was not applied: " + err.Error()
		} else {
			out["templateApplied"] = true
		}
	}
	s.json(w, http.StatusAccepted, out)
}

// localPart is what a person may type before @<login domain>.
var localPart = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// tenantGroupPrefix is the subtree a tenant's own groups live in.
func tenantGroupPrefix(tenant string) string {
	if tenant == gitops.PlatformTenant {
		return "gentian:platform:"
	}
	return "gentian:tenant:" + tenant + ":"
}

// groupLabel is the part of a custom group's name an administrator types.
// No colon, so a label can never reach into the names the platform composes
// (":app:<profile>", ":admin").
var groupLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// reservedGroupLabels are names the platform composes itself.
var reservedGroupLabels = map[string]bool{"admin": true, "admins": true, "app-admins": true, "members": true}

// updatePerson changes somebody's names, delivery address or whether they may
// sign in.
func (s *Server) updatePerson(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Person    string  `json:"person"`
		FirstName *string `json:"firstName"`
		LastName  *string `json:"lastName"`
		Enabled   *bool   `json:"enabled"`
		Email     *string `json:"email"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	person, err := s.cfg.Identity.UpdatePerson(identityContext(r), realm, body.Person, identity.PersonUpdate{
		FirstName: body.FirstName, LastName: body.LastName, Enabled: body.Enabled, Email: body.Email,
	})
	if err != nil {
		if isAddressError(err) {
			s.fail(w, r, http.StatusBadRequest, "that is not an address")
			return
		}
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "update-person", realm, body.Person)
	s.json(w, http.StatusOK, person)
}

// removePerson deletes somebody from the tenant's realm.
func (s *Server) removePerson(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Person string `json:"person"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	// Nobody removes themselves: the screen they are on would be the last
	// thing they could do, and a tenant whose only administrator did it has
	// nobody left to undo it.
	if body.Person != "" && body.Person == c.meta.Subject {
		s.fail(w, r, http.StatusBadRequest, "you cannot remove yourself")
		return
	}
	if err := s.cfg.Identity.RemovePerson(identityContext(r), realm, body.Person); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "remove-person", realm, body.Person)
	s.json(w, http.StatusOK, map[string]any{"person": body.Person, "removed": true})
}

// requireTOTP makes somebody enrol an authenticator, and mails them the link
// when asked to.
func (s *Server) requireTOTP(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Person string `json:"person"`
		Mail   bool   `json:"mail"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	ctx := identityContext(r)
	client := s.inviteClientID(realm)
	redirect := s.cfg.InviteRedirectURI
	if redirect == "" && body.Mail {
		redirect = s.cfg.Identity.ZoneLanding(ctx, realm, client)
	}
	if err := s.cfg.Identity.RequireTOTP(ctx, realm, body.Person, body.Mail, client, redirect); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "require-totp", realm, body.Person)
	s.json(w, http.StatusOK, map[string]any{"person": body.Person, "totpRequired": true, "mailed": body.Mail})
}

// removeTOTP deletes somebody's authenticators, for a lost device.
func (s *Server) removeTOTP(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Person string `json:"person"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if err := s.cfg.Identity.RemoveTOTP(identityContext(r), realm, body.Person); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "remove-totp", realm, body.Person)
	s.json(w, http.StatusOK, map[string]any{"person": body.Person, "totpRequired": false, "totpConfigured": false})
}

// createGroup makes a custom group in the tenant's subtree.
func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	label := strings.ToLower(strings.TrimSpace(body.Name))
	if !groupLabel.MatchString(label) || reservedGroupLabels[label] {
		s.fail(w, r, http.StatusBadRequest, "a group name is lower-case letters, digits and dashes, and not one the platform uses")
		return
	}
	group, err := s.cfg.Identity.CreateGroup(identityContext(r), realm, tenantGroupPrefix(r.PathValue("t"))+label)
	if err != nil {
		if errors.Is(err, identity.ErrConflict) {
			s.fail(w, r, http.StatusConflict, "that group already exists")
			return
		}
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "create-group", realm, group.Path)
	s.json(w, http.StatusCreated, group)
}

// deleteGroup removes a custom group.
func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Group string `json:"group"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if err := s.cfg.Identity.DeleteGroup(identityContext(r), realm, body.Group); err != nil {
		if errors.Is(err, identity.ErrNotCustom) {
			s.fail(w, r, http.StatusBadRequest, "this group is the platform's: it is removed with the app or tenant it belongs to")
			return
		}
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "delete-group", realm, body.Group)
	s.json(w, http.StatusOK, map[string]any{"group": body.Group, "deleted": true})
}

// renameGroup gives a custom group a new label within the tenant's subtree.
func (s *Server) renameGroup(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Group string `json:"group"`
		Name  string `json:"name"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	label := strings.ToLower(strings.TrimSpace(body.Name))
	if !groupLabel.MatchString(label) || reservedGroupLabels[label] {
		s.fail(w, r, http.StatusBadRequest, "a group name is lower-case letters, digits and dashes, and not one the platform uses")
		return
	}
	group, err := s.cfg.Identity.RenameGroup(identityContext(r), realm, body.Group, tenantGroupPrefix(r.PathValue("t"))+label)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrNotCustom):
			s.fail(w, r, http.StatusBadRequest, "this group is the platform's, and keeps its name")
		case errors.Is(err, identity.ErrConflict):
			s.fail(w, r, http.StatusConflict, "a group with that name already exists")
		default:
			s.identityError(w, r, err)
		}
		return
	}
	s.recordIdentityAction(r, c, "rename-group", realm, group.Path)
	s.json(w, http.StatusOK, group)
}

// listGroupMembers answers who is in one group.
func (s *Server) listGroupMembers(w http.ResponseWriter, r *http.Request, _ call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	group := r.URL.Query().Get("group")
	people, err := s.cfg.Identity.GroupMembers(identityContext(r), realm, group)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"group": group, "people": people})
}

// listTemplates answers the desktop's settings templates for this tenant.
//
// They live in the desktop, which keeps a person's settings; the director
// relays the caller's own token there rather than holding a copy, so the
// desktop's own check -- is this a tenant administrator -- still decides.
func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request, _ call) {
	if s.cfg.DesktopAPI == "" {
		s.json(w, http.StatusOK, map[string]any{"templates": []any{}})
		return
	}
	resp, err := s.desktop(r, http.MethodGet, "/prefs/templates", nil)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "the desktop could not be reached")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		s.fail(w, r, http.StatusBadGateway, fmt.Sprintf("the desktop answered %d", resp.StatusCode))
		return
	}
	var raw []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		s.fail(w, r, http.StatusBadGateway, "the desktop's answer could not be read")
		return
	}
	s.json(w, http.StatusOK, map[string]any{"templates": raw})
}

// applyTemplate copies one template onto a person's desktop settings.
func (s *Server) applyTemplate(r *http.Request, template, personID string) error {
	if s.cfg.DesktopAPI == "" {
		return errors.New("this director reaches no desktop")
	}
	body, _ := json.Marshal(map[string]string{"target_user_sub": personID})
	resp, err := s.desktop(r, http.MethodPost, "/prefs/templates/"+url.PathEscape(template)+"/apply", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the desktop answered %d", resp.StatusCode)
	}
	return nil
}

// desktop calls the tenant's desktop API with the caller's own token.
func (s *Server) desktop(r *http.Request, method, path string, body []byte) (*http.Response, error) {
	base := fmt.Sprintf(s.cfg.DesktopAPI, r.PathValue("t"))
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+path, rd)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	return resp, nil
}

// cancelOnClose releases a request's context when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// inviteClientID is the client the invitation link is for.
//
// Keycloak validates an action token against a client, so the link has to name
// one that exists in the realm. The zone's own client is the right one: it is
// what the person will sign in through the moment they have a password.
//
// Derived from the realm rather than configured per tenant, because a
// configured value would be one more thing to write per tenant and one more
// thing to get wrong. The composition names the zone client
// gentian-edge-<zone>, and the zone is the realm's own name in every case but
// one: a cluster whose kernel realm was renamed keeps the zone called "kernel"
// while the realm is called something else. Such a cluster sets the override.
//
// No redirect by default. A redirect must be on the client's valid redirect
// URIs, and the composition lists only each host's /oauth2/callback there --
// which is the OIDC callback and not a page to land on. Sending none leaves
// Keycloak's own "your account has been updated" page, which works and says
// nothing useful; giving the person a way back is the decision recorded
// against M3 rather than one taken here by widening what the zone client
// accepts.
func (s *Server) inviteClientID(realm identity.Realm) string {
	if s.cfg.InviteClientID != "" {
		return s.cfg.InviteClientID
	}
	return "gentian-edge-" + realm.Name()
}

// setMembership adds or removes one person from one group.
func (s *Server) setMembership(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Person string `json:"person"`
		Group  string `json:"group"`
		Member *bool  `json:"member"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.Member == nil {
		s.fail(w, r, http.StatusBadRequest, "say whether they are a member: true or false")
		return
	}
	if err := s.cfg.Identity.SetMembership(identityContext(r), realm,
		body.Person, body.Group, *body.Member); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "set-membership", realm, body.Group)
	s.json(w, http.StatusOK, map[string]any{
		"person": body.Person, "group": body.Group, "member": *body.Member,
	})
}

// sendPasswordReset mails somebody a link to set a new password.
//
// An administrator acting for somebody who cannot act for themselves, which is
// why it is guarded like every other write here. Self-service reset is
// Keycloak's own login page and needs nothing from the director: a locked-out
// person holds no token, so there is no caller to check, and an endpoint that
// skipped the check would be the one unauthenticated write into a source of
// truth.
func (s *Server) sendPasswordReset(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Person string `json:"person"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	ctx := identityContext(r)
	client := s.inviteClientID(realm)
	redirect := s.cfg.InviteRedirectURI
	if redirect == "" {
		redirect = s.cfg.Identity.ZoneLanding(ctx, realm, client)
	}
	if err := s.cfg.Identity.SendPasswordReset(ctx, realm, body.Person, client, redirect); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "send-password-reset", realm, body.Person)
	s.json(w, http.StatusAccepted, map[string]any{"person": body.Person, "mailed": true})
}

// setPasswordPolicy writes the realm's policy.
//
// can_set_policy rather than can_manage_users: this is a statement about the
// tenant rather than about a person, and it sits with the other policies a
// tenant administrator sets.
func (s *Server) setPasswordPolicy(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	var body struct {
		PasswordPolicy *string `json:"passwordPolicy"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.PasswordPolicy == nil {
		s.fail(w, r, http.StatusBadRequest, "passwordPolicy is required; send an empty string to clear it")
		return
	}
	if err := s.cfg.Identity.SetPasswordPolicy(identityContext(r), realm, *body.PasswordPolicy); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "set-password-policy", realm, realm.Name())
	s.json(w, http.StatusOK, map[string]any{"realm": realm.Name(), "passwordPolicy": *body.PasswordPolicy})
}

// recordIdentityAction writes the director's half of the record: the caller,
// the relation and the object that permitted the call, and the request id that
// joins it to Keycloak's admin event for the same change.
//
// Keycloak's event is the better record of WHAT changed, because it is written
// whether the change came through here or through Keycloak's own console.
// This is the better record of the AUTHORITY, because Keycloak sees only this
// director's service account. Neither is sufficient alone, which is why the
// request id matters.
//
// The fields, not the values: what changed and who was allowed to change it,
// never the old and new address. A log that carried those would become the
// problem the decision to keep people out of git avoided.
// recordIdentityAction records WHO WAS ALLOWED to ask for this, which is the
// half of the record Keycloak cannot write: its own admin event sees the
// director's service account and nothing about the person who asked or what
// permitted the call. The two join on the request id (S7A.17).
//
// The log line stays. It is what a person reads while watching a deployment,
// and it is the only record on a cluster whose database has not been
// provisioned yet.
//
// A failure to record does not undo what was done. The caller has already
// invited somebody; refusing to have done it because the record could not be
// written would be the worse outcome, and the gap is visible anyway — a
// Keycloak event with no partner is exactly what an audit looks for.
func (s *Server) recordIdentityAction(r *http.Request, c call, action string, realm identity.Realm, target string) {
	ctx := r.Context()
	s.cfg.Log.InfoContext(ctx, "identity action",
		"request_id", reqID(ctx),
		"action", action,
		"realm", realm.Name(),
		"tenant", r.PathValue("t"),
		"target", target,
		"principal", c.meta.Subject,
		"decision", c.meta.Decision,
	)
	if s.cfg.Record == nil {
		return
	}
	// Not the request's context: it is cancelled the moment the response is
	// written, and the record must not be lost to the caller hanging up.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := s.cfg.Record.Write(writeCtx, record.Action{
		RequestID: reqID(ctx),
		Action:    action,
		Realm:     realm.Name(),
		Tenant:    r.PathValue("t"),
		Target:    target,
		Principal: c.meta.Subject,
		Decision:  c.meta.Decision,
	})
	if err != nil {
		s.cfg.Log.ErrorContext(ctx, "the authority for an identity action was not recorded",
			"request_id", reqID(ctx), "action", action, "error", err.Error())
	}
}

// decode reads a JSON body, refusing anything unreasonable rather than
// letting it reach Keycloak.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		s.fail(w, r, http.StatusBadRequest, "the request body could not be read")
		return false
	}
	return true
}

func isAddressError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not an address")
}
