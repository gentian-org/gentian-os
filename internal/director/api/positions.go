/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"errors"
	"net/http"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// A tenant's own positions (Tenant spec.positions).
//
// Declaring what a position is belongs to whoever manages the tenant's
// people (can_manage_users): it decides what the people given it may do,
// which is the same right as giving it. The declaration is a commit; who
// holds a position is not here, and is given through the registrar.

// shippedPositions are the positions every tenant has, as a list tells
// them from a tenant's own. What each holds follows from what the tenant
// has installed and is the rights store's to say.
var shippedPositions = []string{authz.PositionUser, authz.PositionAppAdministrator, authz.PositionTenantAdministrator}

func (s *Server) tenantPositions(w http.ResponseWriter, r *http.Request, _ call) {
	positions, err := s.cfg.Repo.TenantPositions(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if positions == nil {
		positions = []gentianov1alpha1.TenantPosition{}
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": r.PathValue("t"), "shipped": shippedPositions, "positions": positions,
	})
}

func (s *Server) setTenantPosition(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Title       string                                `json:"title"`
		Description string                                `json:"description"`
		Includes    []string                              `json:"includes"`
		Roles       []gentianov1alpha1.TenantPositionRole `json:"roles"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest,
			`body must be {"title": "...", "description": "...", "includes": ["position", ...], "roles": [{"app": "profile", "role": "user"}, ...]}`)
		return
	}
	res, err := s.cfg.Repo.SetTenantPosition(r.Context(), r.PathValue("t"), gentianov1alpha1.TenantPosition{
		Name: r.PathValue("name"), Title: body.Title, Description: body.Description,
		Includes: body.Includes, Roles: body.Roles,
	}, c.meta)
	if errors.Is(err, gitops.ErrInvalidPosition) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

func (s *Server) removeTenantPosition(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RemoveTenantPosition(r.Context(), r.PathValue("t"), r.PathValue("name"), c.meta)
	switch {
	case errors.Is(err, gitops.ErrNoSuchPosition):
		s.fail(w, r, http.StatusNotFound, "this tenant declares no such position")
	case errors.Is(err, gitops.ErrInvalidPosition):
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
	default:
		s.written(w, r, res, err)
	}
}
