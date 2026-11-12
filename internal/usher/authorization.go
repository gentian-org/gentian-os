/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usher

import (
	"net/http"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/guard"
)

// The read-only view of the authorization state: who holds which role on a
// tenant or on the cluster, and what each role carries. It is read from
// OpenFGA and nothing else, and it exists instead of exposing OpenFGA's own
// playground, which has a write surface. Who holds what is changed on the
// console's other screens, through the director and the registrar.

// tenantAuthorization answers who holds what on one tenant.
func (s *Server) tenantAuthorization(w http.ResponseWriter, r *http.Request, c call) {
	s.authorizationOf(w, r, authz.Tenant(c.tenant))
}

// clusterAuthorization answers who holds what on this cluster.
func (s *Server) clusterAuthorization(w http.ResponseWriter, r *http.Request, _ call) {
	s.authorizationOf(w, r, authz.Cluster(s.cfg.Cluster))
}

func (s *Server) authorizationOf(w http.ResponseWriter, r *http.Request, object string) {
	view, err := s.cfg.Viewer.ViewOf(r.Context(), object)
	if err != nil {
		s.cfg.Log.ErrorContext(r.Context(), "authorization view failed",
			"request_id", guard.ID(r.Context()), "object", object, "error", err.Error())
		fail(w, r, http.StatusBadGateway, "the authorization graph could not be read")
		return
	}
	writeJSON(w, http.StatusOK, view)
}
