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

// The repositories a tenant or the cluster installs from.
//
// Declared state, so a commit: the director writes the Repository into the
// deployments repository as the person who asked, and Argo CD applies it.
// What is declared is the address and nothing that is secret. The credential
// is a requirement the composition emits from the declaration; its value is
// set at the custodian and is read back by nobody.

func (s *Server) declareTenantRepository(w http.ResponseWriter, r *http.Request, c call) {
	s.declareRepository(w, r, c, r.PathValue("t"))
}

func (s *Server) declareClusterRepository(w http.ResponseWriter, r *http.Request, c call) {
	s.declareRepository(w, r, c, "")
}

func (s *Server) removeTenantRepository(w http.ResponseWriter, r *http.Request, c call) {
	s.removeRepository(w, r, c, r.PathValue("t"))
}

func (s *Server) removeClusterRepository(w http.ResponseWriter, r *http.Request, c call) {
	s.removeRepository(w, r, c, "")
}

// declareRepository commits one. tenant is the owner the route named; empty
// is the cluster. It is never taken from the body: whose repository this is
// follows from the object the caller was authorised on.
func (s *Server) declareRepository(w http.ResponseWriter, r *http.Request, c call, tenant string) {
	name := r.PathValue("name")
	var body gitops.RepositoryDeclaration
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"role": "apps|deployments", "type": "git|oci", "url": "…"}`)
		return
	}
	if err := gitops.CheckRepositoryDeclaration(name, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.cfg.Repo.DeclareRepository(r.Context(), tenant, name, body, c.meta)
	if err != nil {
		s.repositoryError(w, r, name, err)
		return
	}
	answer := map[string]any{
		"status":         res.Status,
		"name":           name,
		"tenant":         res.Tenant,
		"role":           body.Role,
		"credentialName": gitops.RepositoryCredentialName(name),
		"created":        res.Created,
	}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	answer["message"] = "committed to the deployments repository; Argo CD applies it on the next sync, " +
		"and the credential can be set at the custodian once it has"
	s.json(w, http.StatusAccepted, answer)
}

// removeRepository commits the removal. The name is repeated in ?confirm=,
// always: there is no additive case to exempt.
func (s *Server) removeRepository(w http.ResponseWriter, r *http.Request, c call, tenant string) {
	name := r.PathValue("name")
	res, err := s.cfg.Repo.RemoveRepository(r.Context(), tenant, name, r.URL.Query().Get("confirm"), c.meta)
	if err != nil {
		s.repositoryError(w, r, name, err)
		return
	}
	answer := map[string]any{"status": res.Status, "name": name, "tenant": tenant, "deleted": true, "commit": res.Commit}
	if tenant == "" {
		// Said in the answer, because "removed" would otherwise claim more
		// than happened: the cluster's own declarations are synced without
		// pruning, so the object stays applied until it is deleted there.
		answer["message"] = "the declaration is removed from the deployments repository. The cluster's own " +
			"declarations are synced without pruning, so the Repository stays in the cluster until it is deleted there"
	} else {
		answer["message"] = "committed to the deployments repository; Argo CD removes it on the next sync"
	}
	s.json(w, http.StatusAccepted, answer)
}

func (s *Server) repositoryError(w http.ResponseWriter, r *http.Request, name string, err error) {
	var confirm *gitops.ConfirmationRequired
	switch {
	case errors.As(err, &confirm):
		// 428, and the same body the custodian answered when this was its
		// route: the console's danger zone reads these fields.
		s.json(w, http.StatusPreconditionRequired, map[string]any{
			"error":          confirm.Reason,
			"confirmField":   "confirm",
			"confirmWith":    confirm.Name,
			"dangerous":      true,
			"requiresRetype": true,
			"request_id":     reqID(r.Context()),
		})
	case errors.Is(err, gitops.ErrRepositoryNotFound):
		// 404 rather than 403 or 409: learning that a name is taken by the
		// cluster or by another tenant is itself a disclosure.
		s.fail(w, r, http.StatusNotFound, "no such repository: "+name)
	case errors.Is(err, gitops.ErrRepositoryNotRewritable):
		s.fail(w, r, http.StatusConflict, err.Error())
	default:
		s.repoError(w, r, err)
	}
}
