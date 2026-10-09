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

// delegatePerimeter and undelegatePerimeter are the switch: whether a
// tenant's own administrators may approve what it publishes to the internet
// (Tenant.spec.perimeter.adminsApprove).
//
// The cluster administrator's alone, which is why there is no such route
// under /v1/tenants. The director commits the manifest; the operator projects
// it, so the right is held a reconcile after the commit is synced and not
// before, and is gone the same way.
//
// Not for the platform tenant: its administrators are the cluster's, who
// approve there as they do in every tenant their cluster operates.
func (s *Server) delegatePerimeter(w http.ResponseWriter, r *http.Request, c call) {
	s.setPerimeterDelegation(w, r, c, true)
}

func (s *Server) undelegatePerimeter(w http.ResponseWriter, r *http.Request, c call) {
	s.setPerimeterDelegation(w, r, c, false)
}

func (s *Server) setPerimeterDelegation(w http.ResponseWriter, r *http.Request, c call, approve bool) {
	tenant := r.PathValue("t")
	res, err := s.cfg.Repo.SetTenantAdminsApprove(r.Context(), tenant, approve, c.meta)
	if errors.Is(err, gitops.ErrPerimeterPlatformTenant) {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	answer := map[string]any{"status": res.Status, "tenant": tenant, "adminsApprove": approve}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	s.json(w, http.StatusAccepted, answer)
}
