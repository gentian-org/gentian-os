/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"errors"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TenantState is what the cluster holds of one tenant right now.
//
// The director asks before the second half of a purge: the manifest it
// switched to deletionPolicy: Delete is removed only once the live Tenant
// carries Delete, or the operator would tear it down under the Retain it last
// saw and keep the data the purge was for.
type TenantState struct {
	Name string `json:"name"`
	// Exists is false for a tenant the cluster does not hold -- answered as
	// 200, so a 404 can only mean an operator without this route, which a
	// caller must not read as "nothing here to keep".
	Exists         bool   `json:"exists"`
	DeletionPolicy string `json:"deletionPolicy"`
	Phase          string `json:"phase,omitempty"`
	// Deleting is true once the Tenant has a deletion timestamp.
	Deleting bool `json:"deleting"`
}

func (h *HTTPServer) registerTenantRoutes(mux router) {
	mux.HandleFunc("GET /v1/tenants/{tenant}", h.handleTenantState)
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/remove-orphan", h.handleRemoveOrphanTenant)
}

// handleRemoveOrphanTenant deletes a Tenant git no longer describes and no
// Argo CD Application owns (orphan_tenant.go). The director asks it when a
// retire finds nothing in git: 202 and what was removed; 404 for a Tenant
// the cluster does not hold; 409 with a reason for one this is not for --
// "not-orphan" (not Argo CD's, or the platform's) or "still-declared"
// (its Application exists, so git is the way).
func (h *HTTPServer) handleRemoveOrphanTenant(w http.ResponseWriter, r *http.Request) {
	res, err := h.Service.RemoveOrphanTenant(r.Context(), r.PathValue("tenant"), actorOf(r))
	switch {
	case apierrors.IsNotFound(err):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, ErrNotOrphan):
		writeJSON(w, http.StatusConflict, map[string]string{"detail": err.Error(), "reason": "not-orphan"})
	case errors.Is(err, ErrTenantDeclared):
		writeJSON(w, http.StatusConflict, map[string]string{"detail": err.Error(), "reason": "still-declared"})
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusAccepted, res)
	}
}

func (h *HTTPServer) handleTenantState(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("tenant")
	var tenant gentianov1alpha1.Tenant
	if err := h.Service.client.Get(r.Context(), types.NamespacedName{Name: name}, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSON(w, http.StatusOK, TenantState{Name: name})
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	policy := string(tenant.Spec.DeletionPolicy)
	if policy == "" {
		policy = string(gentianov1alpha1.DeletionPolicyRetain)
	}
	writeJSON(w, http.StatusOK, TenantState{
		Name:           tenant.Name,
		Exists:         true,
		DeletionPolicy: policy,
		Phase:          string(tenant.Status.Phase),
		Deleting:       !tenant.DeletionTimestamp.IsZero(),
	})
}
