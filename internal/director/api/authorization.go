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
