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
	"net/http"
	"sort"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

// The types add-ons brought, for the director.
//
// The director commits an add-on's objects to git (internal/director/api/
// extensions.go), and only of a type that is an add-on's own, installed and
// granted now: an object of a type whose grant ran out is not declared again.
// It reads git and not the cluster, so it asks this.

// APIExtensionType is one type an add-on brought, as the director needs it
// to write and check an object of it.
type APIExtensionType struct {
	Group    string `json:"group"`
	Resource string `json:"resource"`
	Kind     string `json:"kind"`
	// Version is the version objects are written in: the one stored.
	Version string `json:"version"`
	// Profile is the add-on's.
	Profile string `json:"profile"`
	// Schema is the version's openAPIV3Schema, which an object is checked
	// against before it is committed.
	Schema json.RawMessage `json:"schema,omitempty"`
}

// APIExtensions lists the types of every profile some install of which holds
// a live apiExtensions grant for them, each installed on the cluster as that
// profile's.
func (s *Service) APIExtensions(ctx context.Context) ([]APIExtensionType, error) {
	comps := &gentianov1alpha1.ComponentList{}
	if err := s.client.List(ctx, comps); err != nil {
		return nil, fmt.Errorf("list components: %w", err)
	}
	now := time.Now()
	seen := map[string]bool{}
	out := []APIExtensionType{}
	for i := range comps.Items {
		comp := &comps.Items[i]
		granted := security.GrantedSet(comp.Spec.Privileges, now)
		profile := &gentianov1alpha1.ComponentProfile{}
		if err := s.client.Get(ctx, client.ObjectKey{Name: comp.Spec.ProfileRef.Name}, profile); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get profile %s: %w", comp.Spec.ProfileRef.Name, err)
		}
		p := profile.Privileges()
		if p == nil {
			continue
		}
		for j := range p.APIExtensions {
			req := &p.APIExtensions[j]
			if _, ok := granted[security.PrivilegeRef(security.PrivilegeAPIExtensions, req.Name)]; !ok {
				continue
			}
			defs, err := security.APIExtensionDefinitions(req)
			if err != nil {
				continue
			}
			for _, def := range defs {
				if seen[def.Name] {
					continue
				}
				t, ok, err := s.installedExtensionType(ctx, def.Name, profile.Name)
				if err != nil {
					return nil, err
				}
				if ok {
					seen[def.Name] = true
					out = append(out, t)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Resource < out[j].Resource
	})
	return out, nil
}

// installedExtensionType reads one definition as the cluster has it, by name
// (the operator lists none), and answers it only when it is the profile's.
func (s *Service) installedExtensionType(ctx context.Context, name, profile string) (APIExtensionType, bool, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := s.live().Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return APIExtensionType{}, false, nil
		}
		return APIExtensionType{}, false, fmt.Errorf("get CustomResourceDefinition %s: %w", name, err)
	}
	if crd.Labels[security.APIExtensionLabel] != profile || crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		return APIExtensionType{}, false, nil
	}
	for _, v := range crd.Spec.Versions {
		if !v.Storage {
			continue
		}
		t := APIExtensionType{
			Group: crd.Spec.Group, Resource: crd.Spec.Names.Plural, Kind: crd.Spec.Names.Kind,
			Version: v.Name, Profile: profile,
		}
		if v.Schema != nil && v.Schema.OpenAPIV3Schema != nil {
			raw, err := json.Marshal(v.Schema.OpenAPIV3Schema)
			if err != nil {
				return APIExtensionType{}, false, err
			}
			t.Schema = raw
		}
		return t, true, nil
	}
	return APIExtensionType{}, false, nil
}

func (h *HTTPServer) registerAPIExtensionRoutes(mux router) {
	mux.HandleFunc("GET /v1/api-extensions", h.handleAPIExtensions)
}

func (h *HTTPServer) handleAPIExtensions(w http.ResponseWriter, r *http.Request) {
	out, err := h.Service.APIExtensions(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"types": out})
}
