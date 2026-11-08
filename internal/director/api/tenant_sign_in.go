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
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// A tenant's own groups, and the sign-ins between tenants of this cluster
// (Tenant spec.groups, spec.signInFrom, spec.signInTo). Writing either is the
// cluster's administrators' (can_configure): who signs in to a tenant from
// another, and with which groups, is the cluster's trust, not one tenant's.

func (s *Server) tenantGroups(w http.ResponseWriter, r *http.Request, _ call) {
	groups, err := s.cfg.Repo.TenantGroups(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if groups == nil {
		groups = []string{}
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "groups": groups})
}

func (s *Server) setTenantGroups(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Groups []string `json:"groups"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"groups": ["website-editors", ...]}; an empty list removes them`)
		return
	}
	res, err := s.cfg.Repo.SetTenantGroups(r.Context(), r.PathValue("t"), body.Groups, c.meta)
	if errors.Is(err, gitops.ErrInvalidSignIn) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

func (s *Server) tenantSignIn(w http.ResponseWriter, r *http.Request, _ call) {
	from, to, err := s.cfg.Repo.TenantSignIn(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if from == nil {
		from = []gentianov1alpha1.TenantSignInFrom{}
	}
	if to == nil {
		to = []string{}
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "signInFrom": from, "signInTo": to})
}

// setTenantSignIn lets the people of {source} sign in to {t} with the groups
// given: both tenants' line, in one commit.
func (s *Server) setTenantSignIn(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Groups []string `json:"groups"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"groups": ["members", "website-editors"]}`)
		return
	}
	s.writeTenantSignIn(w, r, c, body.Groups, false)
}

func (s *Server) removeTenantSignIn(w http.ResponseWriter, r *http.Request, c call) {
	s.writeTenantSignIn(w, r, c, nil, true)
}

func (s *Server) writeTenantSignIn(w http.ResponseWriter, r *http.Request, c call, groups []string, remove bool) {
	res, err := s.cfg.Repo.SetTenantSignIn(r.Context(), r.PathValue("t"), r.PathValue("source"), groups, remove, c.meta)
	if errors.Is(err, gitops.ErrInvalidSignIn) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}
