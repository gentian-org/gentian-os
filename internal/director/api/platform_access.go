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

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Who on the cluster holds which role by name (spec.platformAccess.roleHolders
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

// setRoleHolder gives a person, by their subject in their tenant's realm, a
// role. The body says who it is to a person reading the claim, why, and
// until when. A person of a tenant other than the platform's has to be one
// of its administrators, as the store holds its admins group's members.
func (s *Server) setRoleHolder(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Scope     string `json:"scope"`
		Username  string `json:"username"`
		Email     string `json:"email"`
		Reason    string `json:"reason"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := decode(r, &body); err != nil || body.Username == "" || body.Reason == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"username": "<the login in the tenant's realm>", "email": "<address, if any>", `+
			`"reason": "<why>", "expiresAt": "<RFC 3339, if it ends>"}`)
		return
	}
	h := gitops.RoleHolder{
		Role: r.PathValue("role"), Scope: body.Scope, Tenant: r.PathValue("tenant"), Subject: r.PathValue("subject"),
		Username: body.Username, Email: body.Email, Reason: body.Reason, ExpiresAt: body.ExpiresAt,
	}
	if h.Tenant != authz.PlatformTenant {
		admin, err := s.tenantAdministrator(r, h.Tenant, h.Subject)
		if err != nil {
			s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		if !admin {
			s.fail(w, r, http.StatusUnprocessableEntity, "only an administrator of "+h.Tenant+" can be named from that tenant")
			return
		}
	}
	res, err := s.cfg.Repo.SetRoleHolder(r.Context(), h, c.meta)
	if errors.Is(err, gitops.ErrInvalidRoleHolder) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// tenantAdministrator says whether the account subject is a member of the
// tenant's admins group, as the store holds it from Keycloak's events. A
// subject or tenant that cannot be named is not one.
func (s *Server) tenantAdministrator(r *http.Request, tenant, subject string) (bool, error) {
	if !gitops.ValidName(tenant) {
		return false, nil
	}
	user, err := authz.User(subject)
	if err != nil {
		return false, nil
	}
	group, err := authz.Group("gentian:tenant:" + tenant + ":admins")
	if err != nil {
		return false, nil
	}
	return s.cfg.Authz.Check(r.Context(), reqID(r.Context()), user, "member", group)
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
