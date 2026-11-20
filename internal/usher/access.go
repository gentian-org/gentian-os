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
	"regexp"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/guard"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// The access page's three questions: what does this person hold in the
// tenant, what does this group give, and who holds what on this app. Each is
// answered from the rights store and from nothing else, so that what is
// shown is what a check would answer. Nothing here changes a right: people,
// groups and grants are changed on the console's other screens, through the
// registrar and the director.

// profileName is a profile's name as a path may carry it: a DNS label.
var profileName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// personAccess answers what one person holds in the tenant and on each of
// its apps, and through which group.
func (s *Server) personAccess(w http.ResponseWriter, r *http.Request, c call) {
	access, err := s.cfg.Access.PersonAccess(r.Context(), guard.ID(r.Context()), s.cfg.Cluster, c.tenant, r.PathValue("id"))
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

// groupAccess answers what one of the tenant's groups gives its members.
// The group is named as Keycloak names it, in the query: its name holds
// colons.
func (s *Server) groupAccess(w http.ResponseWriter, r *http.Request, c call) {
	group := strings.TrimPrefix(r.URL.Query().Get("group"), "/")
	// Another tenant's group is not this tenant's to ask about, whatever it
	// holds.
	if !strings.HasPrefix(group, keycloak.TenantGroupPrefix(c.tenant)) {
		fail(w, r, http.StatusNotFound, "no such group in this tenant")
		return
	}
	access, err := s.cfg.Access.GroupAccess(r.Context(), c.tenant, group)
	if errors.Is(err, authz.ErrInvalidID) {
		fail(w, r, http.StatusNotFound, "no such group in this tenant")
		return
	}
	if err != nil {
		s.accessFailed(w, r, "group", err)
		return
	}
	writeJSON(w, http.StatusOK, access)
}

// appAccess answers who holds which role on one app of the tenant, and what
// each role carries.
func (s *Server) appAccess(w http.ResponseWriter, r *http.Request, c call) {
	profile := r.PathValue("p")
	if !profileName.MatchString(profile) {
		fail(w, r, http.StatusNotFound, "no such app")
		return
	}
	view, err := s.cfg.Viewer.ViewOf(r.Context(), "app:"+c.tenant+"/"+profile)
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
