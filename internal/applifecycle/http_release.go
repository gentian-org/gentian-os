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
	"errors"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/layout"
)

// ReleasePath is where the director reads the cluster's release: what the
// Cluster claim declares (spec.release), what the operator made of it
// (status.release) and the platform the operator runs (status.platform).
// The director holds no cluster credential, so the claim's status reaches
// it through here.
const ReleasePath = "/v1/release"

var clusterClaimGVK = schema.GroupVersionKind{Group: "gentianos.io", Version: "v1alpha1", Kind: "Cluster"}

// ErrNoClusterClaim is a cluster without a Cluster claim: nothing declares
// its release.
var ErrNoClusterClaim = errors.New("no Cluster claim on this cluster")

// ReleaseView is the answer.
type ReleaseView struct {
	// Spec is spec.release as the claim declares it; nil when absent.
	Spec map[string]any `json:"spec,omitempty"`
	// Status is status.release as the operator wrote it; nil before the
	// operator's first pass.
	Status map[string]any `json:"status,omitempty"`
	// Platform is status.platform: the version and build the operator runs
	// and the newest release the index lists.
	Platform map[string]any `json:"platform,omitempty"`
}

func (h *HTTPServer) registerReleaseRoutes(mux router) {
	mux.Read("GET "+ReleasePath, h.handleRelease)
}

func (h *HTTPServer) handleRelease(w http.ResponseWriter, r *http.Request) {
	view, err := h.Service.Release(r.Context())
	switch {
	case errors.Is(err, ErrNoClusterClaim):
		writeErr(w, http.StatusNotFound, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, view)
	}
}

// Release reads the one Cluster claim's release fields.
func (s *Service) Release(ctx context.Context) (*ReleaseView, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(clusterClaimGVK.GroupVersion().WithKind(clusterClaimGVK.Kind + "List"))
	if err := s.client.List(ctx, list, client.InNamespace(layout.Namespace(layout.Provisioning))); err != nil {
		return nil, fmt.Errorf("list Cluster claims: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, ErrNoClusterClaim
	}
	claim := list.Items[0].Object
	view := &ReleaseView{}
	if m, found, _ := unstructured.NestedMap(claim, "spec", "release"); found {
		view.Spec = m
	}
	if m, found, _ := unstructured.NestedMap(claim, "status", "release"); found {
		view.Status = m
	}
	if m, found, _ := unstructured.NestedMap(claim, "status", "platform"); found {
		view.Platform = m
	}
	return view, nil
}
