/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// A person's own account: their names, where they are signed in, and whether
// they have a second factor.
//
// It is the person's own, as their device passwords are: every route here is
// about the caller, who is the token's subject, and nothing in a request can
// name anybody else. The token must be one of the tenant's own realm, where
// the person is kept.
//
// The address is not changed here. It is where a password link and a second
// factor's link are sent, so whoever could change it could take the account
// over; the tenant's administrators change it. The password is not changed
// here either: the realm's own pages do that, at sign-in and through a
// mailed link.

// maxNameLength is the longest first or last name a person gives themself.
const maxNameLength = 255

// ownAccount is what the caller's account page shows.
type ownAccount struct {
	Username       string `json:"username"`
	Email          string `json:"email"`
	FirstName      string `json:"firstName"`
	LastName       string `json:"lastName"`
	TOTPConfigured bool   `json:"totpConfigured"`
	TOTPRequired   bool   `json:"totpRequired"`
}

func ownAccountOf(p identity.Person) ownAccount {
	return ownAccount{
		Username: p.Username, Email: p.Email, FirstName: p.FirstName, LastName: p.LastName,
		TOTPConfigured: p.TOTPConfigured, TOTPRequired: p.TOTPRequired,
	}
}

// getOwnAccount reads the caller's account.
func (s *Server) getOwnAccount(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok || !s.ownToken(w, r, c, realm) {
		return
	}
	p, err := s.cfg.Identity.Person(identityContext(r), realm, c.subject)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, ownAccountOf(p))
}

// updateOwnProfile changes the caller's first and last name, and nothing
// else.
func (s *Server) updateOwnProfile(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok || !s.ownToken(w, r, c, realm) {
		return
	}
	var body struct {
		FirstName *string `json:"firstName"`
		LastName  *string `json:"lastName"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if body.FirstName == nil && body.LastName == nil {
		s.fail(w, r, http.StatusBadRequest, "name a firstName or a lastName to change")
		return
	}
	for _, v := range []*string{body.FirstName, body.LastName} {
		if v != nil && len(strings.TrimSpace(*v)) > maxNameLength {
			s.fail(w, r, http.StatusBadRequest, "a name is at most 255 characters")
			return
		}
	}
	if _, err := s.cfg.Identity.UpdatePerson(identityContext(r), realm, c.subject,
		identity.PersonUpdate{FirstName: body.FirstName, LastName: body.LastName}); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "update-own-profile", realm, c.subject)
	p, err := s.cfg.Identity.Person(identityContext(r), realm, c.subject)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, ownAccountOf(p))
}

// ownSession is one of the caller's sessions as their account page shows it.
type ownSession struct {
	ID         string    `json:"id"`
	IPAddress  string    `json:"ipAddress,omitempty"`
	Started    time.Time `json:"started"`
	LastAccess time.Time `json:"lastAccess"`
	Clients    []string  `json:"clients"`
	// Current is the session the caller's token belongs to.
	Current bool `json:"current"`
}

// listOwnSessions lists where the caller is signed in.
func (s *Server) listOwnSessions(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok || !s.ownToken(w, r, c, realm) {
		return
	}
	held, err := s.cfg.Identity.Sessions(identityContext(r), realm, c.subject)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	out := make([]ownSession, 0, len(held))
	for _, h := range held {
		out = append(out, ownSession{
			ID: h.ID, IPAddress: h.IPAddress, Started: h.Started, LastAccess: h.LastAccess, Clients: h.Clients,
			Current: c.session != "" && h.ID == c.session,
		})
	}
	s.json(w, http.StatusOK, map[string]any{"sessions": out})
}

// endOwnSession ends one of the caller's sessions.
func (s *Server) endOwnSession(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok || !s.ownToken(w, r, c, realm) {
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if err := s.cfg.Identity.EndSession(identityContext(r), realm, c.subject, body.ID); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "end-own-session", realm, c.subject)
	w.WriteHeader(http.StatusNoContent)
}

// endOwnOtherSessions ends every session of the caller but the one their
// token belongs to. A token that does not say which session it belongs to
// ends nothing: ending them all would sign the caller out of the page that
// asked.
func (s *Server) endOwnOtherSessions(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok || !s.ownToken(w, r, c, realm) {
		return
	}
	if c.session == "" {
		s.fail(w, r, http.StatusBadRequest, "this token names no session, so which one to keep is not known")
		return
	}
	held, err := s.cfg.Identity.Sessions(identityContext(r), realm, c.subject)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	for _, h := range held {
		if h.ID == c.session {
			continue
		}
		if err := s.cfg.Identity.EndSession(identityContext(r), realm, c.subject, h.ID); err != nil && !errors.Is(err, identity.ErrNotFound) {
			s.identityError(w, r, err)
			return
		}
	}
	s.recordIdentityAction(r, c, "end-own-other-sessions", realm, c.subject)
	w.WriteHeader(http.StatusNoContent)
}
