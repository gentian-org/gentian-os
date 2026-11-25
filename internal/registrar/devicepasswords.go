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
	"fmt"
	"net/http"
	"time"

	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// A person's device passwords.
//
// A device password lets a calendar, contact or file program that knows only
// a username and a password reach the tenant's door as the person, for the
// ways the person chose (docs/concepts/bridges.md). It is the person's own:
// every route here is about the caller, who is the token's subject, and
// nothing in a request can name anybody else. The token must be one of the
// tenant's own realm, where the passwords are kept.
//
// Making one asks more than reading or revoking one: a sign-in moments
// before, with whatever second factor the realm asks of the person. A
// session left open on a shared computer does not hand out a credential that
// outlives it. A caller whose sign-in is older is answered as RFC 9470 has a
// resource answer for a step-up: 401, error insufficient_user_authentication,
// and the age the sign-in may have. The page signs the person in again and
// asks once more.

// devicePasswordSignInAge is how long ago the sign-in that makes a device
// password may have been.
const devicePasswordSignInAge = 10 * time.Minute

// listDevicePasswords lists the caller's device passwords, never their secrets.
func (s *Server) listDevicePasswords(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok || !s.ownToken(w, r, c, realm) {
		return
	}
	held, err := s.cfg.Identity.DevicePasswords(identityContext(r), realm, c.subject)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"devicePasswords": held})
}

// createDevicePassword makes a device password for the caller and returns it,
// the one time it is shown.
func (s *Server) createDevicePassword(w http.ResponseWriter, r *http.Request, c call) {
	realm, ok := s.realmFor(w, r)
	if !ok || !s.ownToken(w, r, c, realm) {
		return
	}
	var body struct {
		Name string   `json:"name"`
		Ways []string `json:"ways"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	if c.signedIn == 0 || time.Since(time.Unix(c.signedIn, 0)) > devicePasswordSignInAge {
		age := int(devicePasswordSignInAge.Seconds())
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(
			`Bearer error="insufficient_user_authentication", error_description="Sign in again to create a device password", max_age=%d`, age))
		s.fail(w, r, http.StatusUnauthorized, fmt.Sprintf(
			"a device password is made only within %d minutes of signing in: sign in again and ask once more", age/60))
		return
	}
	made, err := s.cfg.Identity.CreateDevicePassword(identityContext(r), realm, c.subject, body.Name, body.Ways)
	if errors.Is(err, identity.ErrInvalid) {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "create-device-password", realm, c.subject)
	// The secret is in this answer and nowhere else; nothing may keep it.
	w.Header().Set("Cache-Control", "no-store")
	s.json(w, http.StatusCreated, made)
}

// revokeDevicePassword removes one of the caller's device passwords.
func (s *Server) revokeDevicePassword(w http.ResponseWriter, r *http.Request, c call) {
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
	if err := s.cfg.Identity.RevokeDevicePassword(identityContext(r), realm, c.subject, body.ID); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "revoke-device-password", realm, c.subject)
	w.WriteHeader(http.StatusNoContent)
}
