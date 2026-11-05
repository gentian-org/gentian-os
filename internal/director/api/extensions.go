/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// An add-on's objects, as commits.
//
// An add-on brings types of its own once break-glass granted them
// (apiExtensions), and reads its objects without writing them: they are
// declared here, by a person, as everything else of a tenant is
// (gitops/extensions.go). What is a tenant's is written under can_set_policy,
// the relation its backup policy and stores are; what is the cluster's under
// can_configure, into the platform tenant's directory.
//
// Only an object of a type installed and granted now is written: the operator
// says which (internal/applifecycle/api_extensions.go), and the object is held to its
// type's schema before it is committed.

// extensionType is one installed, granted type, as the operator answers it
// (extensionType).
type extensionType struct {
	Group    string          `json:"group"`
	Resource string          `json:"resource"`
	Kind     string          `json:"kind"`
	Version  string          `json:"version"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}

// extensionObjectRequest is what a caller states: the spec, and nothing of
// the object's identity, which is the route's.
type extensionObjectRequest struct {
	Spec map[string]any `json:"spec"`
}

// extensionType answers the installed, granted type the route names, or
// answers the call and false.
func (s *Server) extensionTypeOf(w http.ResponseWriter, r *http.Request) (extensionType, bool) {
	status, body, err := s.cfg.Lifecycle.Get(r.Context(), "/v1/api-extensions", nil)
	if err != nil {
		s.lifecycleError(w, r, err)
		return extensionType{}, false
	}
	if status != http.StatusOK {
		s.relay(w, r, status, body, nil)
		return extensionType{}, false
	}
	var answer struct {
		Types []extensionType `json:"types"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		s.fail(w, r, http.StatusBadGateway, "the operator's list of add-on types could not be read")
		return extensionType{}, false
	}
	group, resource := r.PathValue("group"), r.PathValue("resource")
	for _, t := range answer.Types {
		if t.Group == group && t.Resource == resource {
			return t, true
		}
	}
	s.fail(w, r, http.StatusNotFound, "no add-on type "+resource+"."+group+" is installed and granted on this cluster")
	return extensionType{}, false
}

// platformTenantOf answers the platform tenant, which declares what of an
// add-on is the cluster's, or answers the call and "".
func (s *Server) platformTenantOf(w http.ResponseWriter, r *http.Request) string {
	tenant, err := s.cfg.Repo.PlatformTenant(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return ""
	}
	return tenant
}

func (s *Server) declareTenantExtensionObject(w http.ResponseWriter, r *http.Request, c call) {
	s.declareExtensionObject(w, r, c, r.PathValue("t"))
}

func (s *Server) removeTenantExtensionObject(w http.ResponseWriter, r *http.Request, c call) {
	s.removeExtensionObject(w, r, c, r.PathValue("t"))
}

func (s *Server) declareClusterExtensionObject(w http.ResponseWriter, r *http.Request, c call) {
	if tenant := s.platformTenantOf(w, r); tenant != "" {
		s.declareExtensionObject(w, r, c, tenant)
	}
}

func (s *Server) removeClusterExtensionObject(w http.ResponseWriter, r *http.Request, c call) {
	if tenant := s.platformTenantOf(w, r); tenant != "" {
		s.removeExtensionObject(w, r, c, tenant)
	}
}

// declareExtensionObject writes one object into tenant's directory.
func (s *Server) declareExtensionObject(w http.ResponseWriter, r *http.Request, c call, tenant string) {
	t, ok := s.extensionTypeOf(w, r)
	if !ok {
		return
	}
	var body extensionObjectRequest
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"spec": {…}}`)
		return
	}
	if err := gitops.CheckExtensionSpec(t.Schema, body.Spec); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.cfg.Repo.DeclareExtensionObject(r.Context(), tenant, gitops.ExtensionObject{
		Group: t.Group, Version: t.Version, Kind: t.Kind, Resource: t.Resource,
		Name: r.PathValue("name"), Spec: body.Spec,
	}, c.meta)
	if errors.Is(err, gitops.ErrInvalidExtensionObject) || errors.Is(err, gitops.ErrInvalidName) {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// removeExtensionObject stops declaring one. The tenant's directory is synced
// with pruning, so the commit is the whole of it. The type need not be
// granted any more: what was declared under a grant can always be removed.
func (s *Server) removeExtensionObject(w http.ResponseWriter, r *http.Request, c call, tenant string) {
	res, err := s.cfg.Repo.RemoveExtensionObject(r.Context(), tenant,
		r.PathValue("group"), r.PathValue("resource"), r.PathValue("name"), c.meta)
	switch {
	case errors.Is(err, gitops.ErrInvalidExtensionObject) || errors.Is(err, gitops.ErrInvalidName):
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, gitops.ErrExtensionObjectNotFound):
		s.fail(w, r, http.StatusNotFound, err.Error())
		return
	}
	s.written(w, r, res, err)
}
