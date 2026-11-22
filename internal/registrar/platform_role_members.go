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

	"github.com/gentian-org/gentian-os/internal/guard"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// Who of the platform tenant holds each cluster role: the people of the
// kernel realm the Cluster claim names a role holder
// (spec.platformAccess.roleHolders), the cluster's administrators among
// them, each as the kernel realm knows them now. Read only: who holds a
// role is written on the claim, by the director, and the registrar refuses
// to change those accounts (identity/guard.go).
//
// Under can_audit on the cluster, like every other read of who holds what
// over it.

// platformRoleMembers is one role's line in the answer.
type platformRoleMembers struct {
	// Role is the role as the claim spells it.
	Role string `json:"role"`
	// Members are the role's holders. One the realm could not be asked
	// about, or no longer knows, is listed as the claim names them, and the
	// answer says it is incomplete.
	Members     []identity.Person `json:"members"`
	Unavailable bool              `json:"unavailable,omitempty"`
}

func (s *Server) listPlatformRoleMembers(w http.ResponseWriter, r *http.Request, _ call) {
	if s.cfg.RoleHolders == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "this registrar does not read the cluster's role holders")
		return
	}
	holders, err := s.cfg.RoleHolders(r.Context())
	if err != nil {
		s.cfg.Log.ErrorContext(r.Context(), "role holders not read", "request_id", guard.ID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusServiceUnavailable, "the Cluster claim's role holders could not be read")
		return
	}
	// The kernel realm is the platform tenant's.
	var realm identity.Realm
	t, rerr := s.cfg.Tenants.Tenant(r.Context(), platformTenant)
	if rerr == nil {
		realm, rerr = s.cfg.Identity.Realm(t.Realm)
	}
	if rerr != nil {
		s.cfg.Log.WarnContext(r.Context(), "the kernel realm was not reached",
			"request_id", guard.ID(r.Context()), "error", rerr.Error())
	}
	out := []platformRoleMembers{}
	index := map[string]int{}
	complete := true
	for _, h := range holders {
		if h.Tenant != platformTenant {
			continue
		}
		i, ok := index[h.Role]
		if !ok {
			i = len(out)
			index[h.Role] = i
			out = append(out, platformRoleMembers{Role: h.Role, Members: []identity.Person{}})
		}
		person := identity.Person{ID: h.Subject, Username: h.Username, Email: h.Email}
		if rerr == nil {
			if known, err := s.cfg.Identity.Person(identityContext(r), realm, h.Subject); err == nil {
				person = known
			} else {
				s.cfg.Log.WarnContext(r.Context(), "a role holder was not read from the kernel realm",
					"request_id", guard.ID(r.Context()), "role", h.Role, "subject", h.Subject, "error", err.Error())
				out[i].Unavailable, complete = true, false
			}
		} else {
			out[i].Unavailable, complete = true, false
		}
		out[i].Members = append(out[i].Members, person)
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": s.cfg.Cluster, "complete": complete, "roles": out})
}
