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

// Who could be named a role holder (spec.platformAccess.roleHolders): every
// person of the platform tenant, and the administrators of every other tenant
// of the cluster with a realm of its own. The cluster's administrator picks
// from this list and the director commits the choice; the registrar reads,
// and writes nothing.
//
// Under can_configure on the cluster, the relation the choice is written
// under, and not can_audit: a list of people by name is for whoever assigns
// the role, not for everybody who may look at the cluster.

// roleCandidates is one tenant's line in the answer.
type roleCandidates struct {
	Tenant      string `json:"tenant"`
	DisplayName string `json:"displayName,omitempty"`
	Realm       string `json:"realm"`
	// Platform marks the platform tenant, whose people are all candidates;
	// of every other tenant only its administrators are.
	Platform bool `json:"platform,omitempty"`
	// People is absent when the realm could not be read; the answer then
	// says it is incomplete rather than passing the tenant off as one
	// without candidates.
	People      []identity.Person `json:"people,omitempty"`
	Unavailable bool              `json:"unavailable,omitempty"`
}

// listRoleCandidates answers, tenant by tenant, who could be named: the
// platform tenant's people, and each other tenant's admins group's members.
// A tenant that adopts another realm has no people of its own and is left
// out.
func (s *Server) listRoleCandidates(w http.ResponseWriter, r *http.Request, _ call) {
	tenants, err := s.cfg.Tenants.All(r.Context())
	if err != nil {
		s.tenantError(w, r, err)
		return
	}
	out := []roleCandidates{}
	complete := true
	for _, t := range tenants {
		platform := t.Name == platformTenant
		if !platform && t.Realm != t.Name {
			continue
		}
		entry := roleCandidates{Tenant: t.Name, DisplayName: t.DisplayName, Realm: t.Realm, Platform: platform}
		realm, err := s.cfg.Identity.Realm(t.Realm)
		if err == nil {
			var people []identity.Person
			if platform {
				people, err = s.cfg.Identity.People(identityContext(r), realm, "", 0)
			} else {
				people, err = s.cfg.Identity.GroupMembers(identityContext(r), realm, tenantGroupPrefix(t.Name)+"admins")
			}
			if err == nil {
				if people == nil {
					people = []identity.Person{}
				}
				entry.People = people
				out = append(out, entry)
				continue
			}
		}
		entry.Unavailable = true
		complete = false
		s.cfg.Log.WarnContext(r.Context(), "role candidates not read",
			"request_id", reqID(r.Context()), "tenant", t.Name, "realm", t.Realm, "error", err.Error())
		out = append(out, entry)
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": s.cfg.Cluster, "complete": complete, "tenants": out})
}
