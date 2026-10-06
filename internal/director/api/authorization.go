/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"net/http"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// The read-only view of the authorization state (S7A.8).
//
// It exists instead of exposing OpenFGA's own playground, which is a
// development tool with a write surface. Everything here is a GET. Anything a
// person wants to change is changed on the console's other screens, through
// the director, and lands in git or in Keycloak — there is no second write
// path, and this file adds none.
//
// Guarded by the same relation that governs reading the object it describes:
// can_audit on a cluster, can_view on a tenant. Who holds what is not public
// within a cluster, and a tenant's bindings are the tenant's.

// clusterAuthorization answers who holds which role on this cluster, and what
// each role carries.
func (s *Server) clusterAuthorization(w http.ResponseWriter, r *http.Request, c call) {
	s.authorizationOf(w, r, c, authz.Cluster(s.cfg.Cluster))
}

// tenantAuthorization answers the same for one tenant.
func (s *Server) tenantAuthorization(w http.ResponseWriter, r *http.Request, c call) {
	s.authorizationOf(w, r, c, authz.Tenant(r.PathValue("t")))
}

func (s *Server) authorizationOf(w http.ResponseWriter, r *http.Request, _ call, object string) {
	view, err := s.cfg.Viewer.ViewOf(r.Context(), object)
	if err != nil {
		s.cfg.Log.ErrorContext(r.Context(), "authorization view failed",
			"request_id", reqID(r.Context()), "object", object, "error", err.Error())
		s.fail(w, r, http.StatusBadGateway, "the authorization graph could not be read")
		return
	}
	s.json(w, http.StatusOK, view)
}
