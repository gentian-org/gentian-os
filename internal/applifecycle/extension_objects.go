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
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// An add-on's objects, read back.
//
// The director writes them to git (internal/director/api/extensions.go) and
// the add-on's component writes their status; what a person is shown is the
// two together, as the cluster holds them. Only of a type that is an
// add-on's own, installed and granted now (APIExtensions), and only in the
// namespace of the tenant asked about: the operator reads them through
// gentian-api-extensions-reader, a role of these types alone.

// ExtensionObject is one object of an add-on's type: what was declared and
// what the add-on reports of it.
type ExtensionObject struct {
	Name    string          `json:"name"`
	Created string          `json:"created"`
	Spec    json.RawMessage `json:"spec,omitempty"`
	Status  json.RawMessage `json:"status,omitempty"`
}

// ExtensionObjects is the answer for one tenant and one type.
type ExtensionObjects struct {
	Tenant string            `json:"tenant"`
	Type   APIExtensionType  `json:"type"`
	Items  []ExtensionObject `json:"items"`
}

// errNotAnExtension is a group and resource no add-on brought, or whose
// grant has run out.
var errNotAnExtension = errors.New("no add-on's type, installed and granted, is named so")

// extensionName is what a group (a DNS subdomain) and a resource (a DNS
// label) may look like. Checked before either is asked about.
var (
	extensionGroup    = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	extensionResource = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	extensionTenant   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,50}[a-z0-9])?$`)
)

// ExtensionObjects lists the objects of one add-on type in a tenant's
// namespace, by name.
func (s *Service) ExtensionObjects(ctx context.Context, tenant, group, resource string) (*ExtensionObjects, error) {
	if !extensionTenant.MatchString(tenant) || !extensionGroup.MatchString(group) || !extensionResource.MatchString(resource) {
		return nil, errNotAnExtension
	}
	types, err := s.APIExtensions(ctx)
	if err != nil {
		return nil, err
	}
	var t *APIExtensionType
	for i := range types {
		if types[i].Group == group && types[i].Resource == resource {
			t = &types[i]
			break
		}
	}
	if t == nil {
		return nil, errNotAnExtension
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: t.Group, Version: t.Version, Kind: t.Kind + "List"})
	if err := s.live().List(ctx, list, client.InNamespace(layout.Tenant(tenant))); err != nil {
		return nil, fmt.Errorf("list %s.%s: %w", t.Resource, t.Group, err)
	}
	out := &ExtensionObjects{Tenant: tenant, Type: *t, Items: []ExtensionObject{}}
	out.Type.Schema = nil
	for i := range list.Items {
		o := &list.Items[i]
		item := ExtensionObject{Name: o.GetName(), Created: o.GetCreationTimestamp().UTC().Format("2006-01-02T15:04:05Z")}
		for field, into := range map[string]*json.RawMessage{"spec": &item.Spec, "status": &item.Status} {
			if v, ok := o.Object[field]; ok {
				raw, err := json.Marshal(v)
				if err != nil {
					return nil, err
				}
				*into = raw
			}
		}
		out.Items = append(out.Items, item)
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	return out, nil
}

func (h *HTTPServer) registerExtensionObjectRoutes(mux router) {
	// A tenant's objects, and the cluster's: those of the platform tenant,
	// where the director writes what is the cluster's.
	mux.Read("GET /v1/tenants/{tenant}/extensions/{group}/{resource}", h.handleExtensionObjects)
	mux.Read("GET /v1/extensions/{group}/{resource}", h.handleExtensionObjects)
}

func (h *HTTPServer) handleExtensionObjects(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	if tenant == "" {
		tenant = gentianov1alpha1.PlatformTenantName
	}
	out, err := h.Service.ExtensionObjects(r.Context(), tenant, r.PathValue("group"), r.PathValue("resource"))
	switch {
	case errors.Is(err, errNotAnExtension):
		writeErr(w, http.StatusNotFound, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}
