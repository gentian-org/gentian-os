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

// The kernel realm as hub for the tenants' administrators (iam.md 1.1c):
// who holds cluster#security_officer, declared on the Cluster claim by the
// cluster's administrator, and whether a tenant lets the platform's
// administrators sign in to it, declared on its manifest by its own.
//
// Both are commits. The director writes nothing to the authorization graph
// here: the platform tenant's composition renders the linked account and its
// membership from the claim, Keycloak's events state the membership, and the
// operator writes the tuple.

// securityOfficers answers who the claim names, and where each signs in.
func (s *Server) securityOfficers(w http.ResponseWriter, r *http.Request, _ call) {
	officers, err := s.cfg.Repo.SecurityOfficers(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	kd, _ := s.cfg.Repo.KernelDomain(r.Context())
	out := make([]map[string]any, 0, len(officers))
	for _, o := range officers {
		entry := map[string]any{
			"tenant": o.Tenant, "subject": o.Subject, "username": o.Username, "email": o.Email,
			// The identity provider the kernel realm has for the tenant, and
			// the address that goes straight to it.
			"identityProvider": "tenant-" + o.Tenant,
		}
		if kd != "" {
			entry["signIn"] = "https://platform." + kd + "/?kc_idp_hint=tenant-" + o.Tenant
		}
		out = append(out, entry)
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": r.PathValue("c"), "securityOfficers": out})
}

// setSecurityOfficer names a tenant's administrator, by the subject of their
// account in the tenant's realm, a security officer. The body carries what
// the kernel realm shows: the login, and the address if there is one.
func (s *Server) setSecurityOfficer(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	if err := decode(r, &body); err != nil || body.Username == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"username": "<the login in the tenant's realm>", "email": "<address, if any>"}`)
		return
	}
	o := gitops.SecurityOfficer{
		Tenant: r.PathValue("tenant"), Subject: r.PathValue("subject"),
		Username: body.Username, Email: body.Email,
	}
	res, err := s.cfg.Repo.SetSecurityOfficer(r.Context(), o, c.meta)
	if errors.Is(err, gitops.ErrInvalidSecurityOfficer) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

func (s *Server) removeSecurityOfficer(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RemoveSecurityOfficer(r.Context(), r.PathValue("tenant"), r.PathValue("subject"), c.meta)
	if errors.Is(err, gitops.ErrInvalidSecurityOfficer) {
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
