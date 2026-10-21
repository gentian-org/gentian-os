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

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Who on the cluster holds which cluster role (spec.platformAccess.roleHolders
// on the Cluster claim), declared by the cluster's administrator, and whether
// a tenant lets the platform's administrators sign in to it, declared on its
// manifest by its own.
//
// Both are commits. The director writes nothing to the authorization graph
// here: the operator projects the claim's holders into it.

// roleHolders answers who the claim names.
func (s *Server) roleHolders(w http.ResponseWriter, r *http.Request, _ call) {
	holders, err := s.cfg.Repo.RoleHolders(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": r.PathValue("c"), "roles": gitops.HolderRoleNames(), "roleHolders": holders})
}

// setRoleHolder gives an account of a tenant's realm, by its subject there,
// a cluster role. The body says who it is to a person reading the claim.
func (s *Server) setRoleHolder(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	if err := decode(r, &body); err != nil || body.Username == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"username": "<the login in the tenant's realm>", "email": "<address, if any>"}`)
		return
	}
	h := gitops.RoleHolder{
		Role: r.PathValue("role"), Tenant: r.PathValue("tenant"), Subject: r.PathValue("subject"),
		Username: body.Username, Email: body.Email,
	}
	res, err := s.cfg.Repo.SetRoleHolder(r.Context(), h, c.meta)
	if errors.Is(err, gitops.ErrInvalidRoleHolder) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

func (s *Server) removeRoleHolder(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RemoveRoleHolder(r.Context(), r.PathValue("role"), r.PathValue("tenant"), r.PathValue("subject"), c.meta)
	if errors.Is(err, gitops.ErrInvalidRoleHolder) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// tenantPlatformAccess answers whether the platform's administrators may
// sign in to the tenant.
func (s *Server) tenantPlatformAccess(w http.ResponseWriter, r *http.Request, _ call) {
	on, err := s.cfg.Repo.TenantPlatformAccess(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "adminsEnter": on})
}

// setTenantPlatformAccess writes the switch.
func (s *Server) setTenantPlatformAccess(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		AdminsEnter *bool `json:"adminsEnter"`
	}
	if err := decode(r, &body); err != nil || body.AdminsEnter == nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"adminsEnter": true} or {"adminsEnter": false}`)
		return
	}
	res, err := s.cfg.Repo.SetTenantPlatformAccess(r.Context(), r.PathValue("t"), *body.AdminsEnter, c.meta)
	if errors.Is(err, gitops.ErrPlatformAccessPlatformTenant) {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	s.written(w, r, res, err)
}
