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

// The store's sign-in through the tenant's own realm (Tenant
// spec.store.signIn, store-contract.md §6.1). Read by whoever may view the
// tenant; written by its administrators (can_administer), since it lets a
// realm outside the cluster learn who its people are when they sign in to
// the store. The App Store app asks for it on the administrator's behalf once
// the store has associated the tenant with their account.

func (s *Server) tenantStoreSignIn(w http.ResponseWriter, r *http.Request, _ call) {
	issuer, err := s.cfg.Repo.TenantStoreSignIn(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	var out any
	if issuer != "" {
		out = issuer
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "enabled": issuer != "", "issuer": out})
}

func (s *Server) setTenantStoreSignIn(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Issuer string `json:"issuer"`
	}
	if err := decode(r, &body); err != nil || body.Issuer == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"issuer": "https://<host>/<path to the store's realm>"}, the issuer the store's GET /v1/meta names`)
		return
	}
	res, err := s.cfg.Repo.SetTenantStoreSignIn(r.Context(), r.PathValue("t"), body.Issuer, c.meta)
	if errors.Is(err, gitops.ErrInvalidStoreSignIn) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

func (s *Server) removeTenantStoreSignIn(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.SetTenantStoreSignIn(r.Context(), r.PathValue("t"), "", c.meta)
	s.written(w, r, res, err)
}
