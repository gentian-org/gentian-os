/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usher

import (
	"errors"
	"net/http"

	"github.com/gentian-org/gentian-os/internal/openfga"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/guard"
)

// The access page's three questions: what does this person hold in the
// tenant, what is this position, and who holds what on this app. Each is
// answered from the rights store and from nothing else, so that what is
// shown is what a check would answer. Nothing here changes a right.

// personAccess answers what one person holds in the tenant and on each of
// its apps, and through which position.
func (s *Server) personAccess(w http.ResponseWriter, r *http.Request, c call) {
	access, err := s.cfg.Access.PersonAccess(openfga.WithFastAnswer(r.Context()), guard.ID(r.Context()), s.cfg.Cluster, c.tenant, r.PathValue("id"))
	if errors.Is(err, authz.ErrInvalidID) {
		fail(w, r, http.StatusNotFound, "no such person")
		return
	}
	if err != nil {
		s.accessFailed(w, r, "person", err)
		return
	}
	writeJSON(w, http.StatusOK, access)
}

// positionAccess answers what one of the tenant's positions is: the roles
// it holds, what it includes, and who was given it. The tenant in the path
// is the only one read: a position is named within its tenant.
func (s *Server) positionAccess(w http.ResponseWriter, r *http.Request, c call) {
	access, err := s.cfg.Access.PositionAccess(openfga.WithFastAnswer(r.Context()), c.tenant, r.PathValue("name"))
	if errors.Is(err, authz.ErrInvalidID) {
		fail(w, r, http.StatusNotFound, "no such position in this tenant")
		return
	}
	if err != nil {
		s.accessFailed(w, r, "position", err)
		return
	}
	writeJSON(w, http.StatusOK, access)
}

// appAccess answers who holds which role on one app of the tenant, and what
// each role carries.
func (s *Server) appAccess(w http.ResponseWriter, r *http.Request, c call) {
	view, err := s.cfg.Access.AppAccess(openfga.WithFastAnswer(r.Context()), c.tenant, r.PathValue("p"))
	if errors.Is(err, authz.ErrInvalidID) {
		fail(w, r, http.StatusNotFound, "no such app")
		return
	}
	if err != nil {
		s.accessFailed(w, r, "app", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) accessFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.cfg.Log.ErrorContext(r.Context(), "access could not be read",
		"request_id", guard.ID(r.Context()), "of", what, "error", err.Error())
	fail(w, r, http.StatusBadGateway, "the authorization graph could not be read")
}
