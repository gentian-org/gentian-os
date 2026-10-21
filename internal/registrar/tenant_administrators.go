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

// Who could be made a security officer: the administrators of every tenant
// of the cluster (iam.md 1.1c). The cluster's administrator picks from this
// list and the director commits the choice; the registrar reads, and writes
// nothing.
//
// Under can_configure on the cluster, the relation the choice is written
// under, and not can_audit: a list of every tenant's administrators by name
// is for whoever assigns the role, not for everybody who may look at the
// cluster.

// tenantAdministrators is one tenant's line in the answer.
type tenantAdministrators struct {
	Tenant      string `json:"tenant"`
	DisplayName string `json:"displayName,omitempty"`
	Realm       string `json:"realm"`
	// Administrators is absent when the realm could not be read; the answer
	// then says it is incomplete rather than passing the tenant off as one
	// without administrators.
	Administrators []identity.Person `json:"administrators,omitempty"`
	Unavailable    bool              `json:"unavailable,omitempty"`
}

// listTenantAdministrators answers the members of each tenant's admins group,
// tenant by tenant. The platform tenant is left out: its people are the
// kernel realm's already, and a security officer is an administrator of
// another tenant.
func (s *Server) listTenantAdministrators(w http.ResponseWriter, r *http.Request, _ call) {
	tenants, err := s.cfg.Tenants.All(r.Context())
	if err != nil {
		s.tenantError(w, r, err)
		return
	}
	out := []tenantAdministrators{}
	complete := true
	for _, t := range tenants {
		if t.Name == platformTenant || t.Realm != t.Name {
			continue
		}
		entry := tenantAdministrators{Tenant: t.Name, DisplayName: t.DisplayName, Realm: t.Realm}
		realm, err := s.cfg.Identity.Realm(t.Realm)
		if err == nil {
			var people []identity.Person
			if people, err = s.cfg.Identity.GroupMembers(identityContext(r), realm, tenantGroupPrefix(t.Name)+"admins"); err == nil {
				if people == nil {
					people = []identity.Person{}
				}
				entry.Administrators = people
				out = append(out, entry)
				continue
			}
		}
		entry.Unavailable = true
		complete = false
		s.cfg.Log.WarnContext(r.Context(), "tenant administrators not read",
			"request_id", reqID(r.Context()), "tenant", t.Name, "realm", t.Realm, "error", err.Error())
		out = append(out, entry)
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": s.cfg.Cluster, "complete": complete, "tenants": out})
}
