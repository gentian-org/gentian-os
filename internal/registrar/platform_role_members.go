/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import (
	"net/http"

	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// Who holds each platform role through its group: the members of the
// kernel-realm group the Cluster claim names for the role
// (spec.platformRoles). Read only. Those groups are managed in Keycloak and
// the registrar refuses to change them (identity/guard.go); this says who is
// in them, beside the named role holders the director reads from the claim.
//
// Under can_audit on the cluster, like every other read of who holds what
// over it.

// platformRoleMembers is one role's line in the answer.
type platformRoleMembers struct {
	RoleGroup
	// Members is null when the group could not be read; the answer then
	// says it is incomplete rather than passing the role off as unheld.
	Members     []identity.Person `json:"members"`
	Unavailable bool              `json:"unavailable,omitempty"`
}

func (s *Server) listPlatformRoleMembers(w http.ResponseWriter, r *http.Request, _ call) {
	if s.cfg.PlatformRoles == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "this registrar does not read the platform roles")
		return
	}
	roles, err := s.cfg.PlatformRoles(r.Context())
	if err != nil {
		s.cfg.Log.ErrorContext(r.Context(), "platform roles not read", "request_id", reqID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusServiceUnavailable, "the Cluster claim's platform roles could not be read")
		return
	}
	// The kernel realm is the platform tenant's.
	var realm identity.Realm
	t, err := s.cfg.Tenants.Tenant(r.Context(), platformTenant)
	if err == nil {
		realm, err = s.cfg.Identity.Realm(t.Realm)
	}
	out := []platformRoleMembers{}
	complete := true
	for _, role := range roles {
		entry := platformRoleMembers{RoleGroup: role}
		if err == nil {
			people, merr := s.cfg.Identity.GroupMembers(identityContext(r), realm, role.Group)
			if merr == nil {
				if people == nil {
					people = []identity.Person{}
				}
				entry.Members = people
				out = append(out, entry)
				continue
			}
			s.cfg.Log.WarnContext(r.Context(), "platform role members not read",
				"request_id", reqID(r.Context()), "role", role.Role, "group", role.Group, "error", merr.Error())
		}
		entry.Unavailable = true
		complete = false
		out = append(out, entry)
	}
	if err != nil {
		s.cfg.Log.WarnContext(r.Context(), "the kernel realm was not reached",
			"request_id", reqID(r.Context()), "error", err.Error())
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": s.cfg.Cluster, "complete": complete, "roles": out})
}
