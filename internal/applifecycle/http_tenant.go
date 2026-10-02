/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package applifecycle

import (
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
