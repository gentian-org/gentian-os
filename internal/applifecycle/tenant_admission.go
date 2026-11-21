/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// Whether the cluster would take a Tenant, asked before it is written.
//
// The director commits a tenant's manifest to git, and Argo CD applies it
// server-side. What the cluster then refuses -- the Tenant webhook's checks,
// a field or a value the schema does not take -- stops every later change to
// that tenant, and the person who asked was told "committed". So the director
// asks first: the manifest is applied here the way Argo CD applies it, as a
// dry run, and the API server's answer is the answer. Nothing is stored.

// TenantAdmissionPath is the route for one tenant.
func TenantAdmissionPath(tenant string) string {
	return "/v1/tenants/" + tenant + "/actions/admit"
}

// TenantAdmission is the cluster's answer about one Tenant manifest.
type TenantAdmission struct {
	// Admitted is false where the cluster would refuse the manifest.
	Admitted bool `json:"admitted"`
	// Reason is the API server's refusal, as it gave it.
	Reason string `json:"reason,omitempty"`
}

// tenantAdmissionOwner is the field manager the dry run applies as. It never
// owns anything: nothing of a dry run is stored.
const tenantAdmissionOwner = "gentian-tenant-admission"

func (h *HTTPServer) registerTenantAdmissionRoutes(mux router) {
	mux.HandleFunc("POST "+TenantAdmissionPath("{tenant}"), h.handleAdmitTenant)
}

// handleAdmitTenant answers 200 with the cluster's answer, 400 for a body
// that is no manifest of the tenant named, and 502 where the cluster could
// not be asked or answered something that is no verdict on the manifest.
func (h *HTTPServer) handleAdmitTenant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Manifest string `json:"manifest"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil || body.Manifest == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(`send {"manifest": "<the Tenant's YAML>"}`))
		return
	}
	answer, err := h.Service.AdmitTenant(r.Context(), r.PathValue("tenant"), []byte(body.Manifest))
	switch {
	case apierrors.IsBadRequest(err):
		writeErr(w, http.StatusBadRequest, err)
	case err != nil:
		writeErr(w, http.StatusBadGateway, err)
	default:
		writeJSON(w, http.StatusOK, answer)
	}
}

// AdmitTenant applies the manifest of the Tenant named as a dry run and
// reports whether the cluster took it.
func (s *Service) AdmitTenant(ctx context.Context, name string, manifest []byte) (TenantAdmission, error) {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(manifest, &obj.Object); err != nil {
		return TenantAdmission{}, apierrors.NewBadRequest("the manifest does not parse: " + err.Error())
	}
	if obj.GetKind() != "Tenant" || obj.GetName() != name {
		return TenantAdmission{}, apierrors.NewBadRequest(fmt.Sprintf("the manifest is not the Tenant %q", name))
	}
	//nolint:staticcheck // An unstructured manifest is applied as Argo CD applies it.
	err := s.client.Patch(ctx, obj, client.Apply, client.DryRunAll, client.ForceOwnership, client.FieldOwner(tenantAdmissionOwner))
	if err == nil {
		return TenantAdmission{Admitted: true}, nil
	}
	if reason, refused := tenantRefusal(err); refused {
		return TenantAdmission{Reason: reason}, nil
	}
	return TenantAdmission{}, err
}

// tenantRefusal tells a verdict on the manifest from an answer that is none:
// an admission webhook's denial, a value the schema does not take, and a
// field it does not declare, which a server-side apply is refused for with
// an internal error. Anything else -- the API server unreachable, this
// operator not allowed -- says nothing about the manifest.
func tenantRefusal(err error) (string, bool) {
	message := err.Error()
	switch {
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return message, true
	case apierrors.IsForbidden(err) && strings.Contains(message, "admission webhook"):
		return message, true
	case strings.Contains(message, "field not declared in schema"):
		return message, true
	}
	return "", false
}
