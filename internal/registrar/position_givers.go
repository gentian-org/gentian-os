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
	"slices"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"

	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/guard"
)

// Positions a component gives.
//
// A tenant may let a component, of its own or of another tenant, give its
// people some of its positions and take them again: a shop's checkout that
// makes a buyer a customer, a course catalog that makes a participant a
// student. The tenant names the component and the positions in
// spec.positionGivers; the operator lists the component's key with them
// (internal/controller/position_givers.go).
//
// The component presents the key it holds for acting on the platform, on
// two routes of their own, and is admitted there for exactly the positions
// the tenant in the path names for it: no other tenant's, no other
// position, never one that holds admin on the tenant or an app (the operator
// lists those apart), and no role. Nothing else is opened
// by the key here, and no person's token opens these routes. The act itself
// is the one a person who manages the tenant's people asks for: the person
// is looked up in the tenant's realm, and the operator gives or takes and
// records who acted, the component by its name.

// PositionGiver is one component tenants let give positions, as the
// operator lists it for the registrar (the ConfigMap
// registrar-position-givers). Its Component is the profile it runs.
type PositionGiver = componentkeys.PositionGiver

// PositionGivers is the list the registrar currently holds, followed as
// PositionGiversList. The zero value, and nil, hold none.
type PositionGivers = componentkeys.List[PositionGiver]

// PositionGiversList names the list in the log and in a refusal.
const PositionGiversList = "position givers"

// ParsePositionGivers reads the operator's list, refused whole by
// componentkeys.Check.
func ParsePositionGivers(raw []byte) ([]PositionGiver, error) {
	return componentkeys.Parse[PositionGiver](raw, PositionGiversList)
}

// positionBody is the person and the position a component gives or takes.
type positionBody struct {
	Person   string `json:"person"`
	Position string `json:"position"`
}

// positionGiving registers a route a component gives or takes a position
// on. It is the one registration besides guarded and linkHolders, and
// admits a component's key and nothing else.
func (s *Server) positionGiving(pattern string, give bool) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		bearer := guard.Bearer(r)
		if bearer == "" {
			s.fail(w, r, http.StatusUnauthorized, "unauthenticated")
			return
		}
		giver := s.cfg.PositionGivers.Find(bearer)
		if giver == nil {
			s.cfg.Log.WarnContext(ctx, "refused: a caller that is no position giver asked to give or take a position",
				"request_id", guard.ID(ctx), "route", pattern, "tenant", r.PathValue("t"))
			s.fail(w, r, http.StatusForbidden,
				"forbidden: positions are given here by a component a tenant names in spec.positionGivers, with its own key, and by nobody else")
			return
		}
		tenant := r.PathValue("t")
		if !gentianov1alpha1.ValidTenantName(tenant) {
			s.fail(w, r, http.StatusBadRequest, "invalid name")
			return
		}
		var body positionBody
		if !s.decode(w, r, &body) {
			return
		}
		if administering(giver, tenant, body.Position) {
			s.cfg.Log.WarnContext(ctx, "refused: a position giver asked for a position that administers",
				"request_id", guard.ID(ctx), "route", pattern, "key_tenant", giver.Tenant, "key_component", giver.Component,
				"tenant", tenant, "position", body.Position)
			s.fail(w, r, http.StatusForbidden,
				"forbidden: "+body.Position+" holds admin on the tenant or on an app, itself or through a position it includes, "+
					"and a component never gives or takes such a position")
			return
		}
		if !gives(giver, tenant, body.Position) {
			s.cfg.Log.WarnContext(ctx, "refused: a position giver asked for a position the tenant does not let it give",
				"request_id", guard.ID(ctx), "route", pattern, "key_tenant", giver.Tenant, "key_component", giver.Component,
				"tenant", tenant, "position", body.Position)
			s.fail(w, r, http.StatusForbidden,
				"forbidden: a component gives and takes the positions a tenant names for it in spec.positionGivers, and no others")
			return
		}
		c := call{
			component: "component:" + giver.Tenant + "/" + giver.Component,
			decision:  "position_giver position:" + tenant + "/" + body.Position,
		}
		s.assignTo(w, r, c, give, assignmentBody{Person: body.Person, Position: body.Position})
	})
}

// administering reports whether position administers in tenant, as the
// operator listed it beside the giver, or is an administrator's.
func administering(giver *PositionGiver, tenant, position string) bool {
	if position == authz.PositionTenantAdministrator || position == authz.PositionAppAdministrator {
		return true
	}
	for _, g := range giver.Gives {
		if g.Tenant == tenant && slices.Contains(g.Administering, position) {
			return true
		}
	}
	return false
}

// gives reports whether the giver may give position in tenant: the tenant
// names it with that position, and it is not an administrator's.
func gives(giver *PositionGiver, tenant, position string) bool {
	if position == "" || position == authz.PositionTenantAdministrator || position == authz.PositionAppAdministrator {
		return false
	}
	for _, g := range giver.Gives {
		if g.Tenant == tenant && slices.Contains(g.Positions, position) {
			return true
		}
	}
	return false
}
