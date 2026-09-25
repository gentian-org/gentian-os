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
	"net/http"
	"strconv"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/identity"
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
	if realm != tenant {
		token = token.Scoped("gentian:tenant:" + tenant + ":")
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
	s.json(w, http.StatusOK, map[string]any{
		"tenant":         r.PathValue("t"),
		"realm":          realm.Name(),
		"passwordPolicy": policy,
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
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	person, err := s.cfg.Identity.Invite(identityContext(r), realm, identity.Invitation{
		Email:  body.Email,
		Groups: body.Groups,
		// The zone's own client and host, so the link lands where they
		// already are rather than on a realm page with no way back.
		ClientID:    s.cfg.InviteClientID,
		RedirectURI: s.cfg.InviteRedirectURI,
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
	s.json(w, http.StatusAccepted, map[string]any{"person": person, "mailed": true})
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
func (s *Server) recordIdentityAction(r *http.Request, c call, action string, realm identity.Realm, target string) {
	s.cfg.Log.InfoContext(r.Context(), "identity action",
		"request_id", reqID(r.Context()),
		"action", action,
		"realm", realm.Name(),
		"tenant", r.PathValue("t"),
		"target", target,
		"principal", c.meta.Subject,
		"decision", c.meta.Decision,
	)
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
