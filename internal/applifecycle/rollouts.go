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
	"net/http"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Rollout is one installed app as the cluster runs it: the build it is
// pinned to, the build it runs, and how the last move between the two went
// (component_move.go in the operator). What the director's report of
// outdated apps reads, beside what git says and what the catalog offers.
type Rollout struct {
	Tenant  string `json:"tenant"`
	Profile string `json:"profile"`
	// Pinned is spec.profileRef.digest: what was asked.
	Pinned string `json:"pinned,omitempty"`
	// Running is the build the instance runs; empty before the first build
	// has rolled out, or for an install pinned to nothing.
	Running string `json:"running,omitempty"`
	// Target, Phase, Reason and Message are the move under way or the last
	// one.
	Target  string `json:"target,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// MovedAt and MovedBy are the request the pin answers.
	MovedAt string `json:"movedAt,omitempty"`
	MovedBy string `json:"movedBy,omitempty"`
	// Ready is the instance's Ready condition, with its reason.
	Ready       bool   `json:"ready"`
	ReadyReason string `json:"readyReason,omitempty"`
}

// Rollouts lists every tenant's installed apps.
type Rollouts struct {
	Rollouts []Rollout `json:"rollouts"`
}

// ListRollouts reads every Component of every tenant.
func (s *Service) ListRollouts(ctx context.Context) (*Rollouts, error) {
	tenants := &gentianov1alpha1.TenantList{}
	if err := s.client.List(ctx, tenants); err != nil {
		return nil, err
	}
	byNamespace := map[string]string{}
	for i := range tenants.Items {
		byNamespace[tenants.Items[i].NamespaceName()] = tenants.Items[i].Name
	}
	comps := &gentianov1alpha1.ComponentList{}
	if err := s.client.List(ctx, comps); err != nil {
		return nil, err
	}
	out := &Rollouts{Rollouts: []Rollout{}}
	for i := range comps.Items {
		comp := &comps.Items[i]
		tenant, ok := byNamespace[comp.Namespace]
		if !ok || comp.DeletionTimestamp != nil {
			continue
		}
		r := Rollout{Tenant: tenant, Profile: comp.Spec.ProfileRef.Name, Pinned: comp.Spec.ProfileRef.Digest,
			MovedAt: comp.Spec.MovedAt, MovedBy: comp.Spec.MovedBy}
		if ro := comp.Status.Rollout; ro != nil {
			r.Running, r.Target, r.Phase, r.Reason, r.Message = ro.Digest, ro.Target, string(ro.Phase), ro.Reason, ro.Message
		}
		for _, c := range comp.Status.Conditions {
			if c.Type == "Ready" {
				r.Ready = c.Status == metav1.ConditionTrue
				r.ReadyReason = c.Reason
			}
		}
		out.Rollouts = append(out.Rollouts, r)
	}
	sort.Slice(out.Rollouts, func(i, j int) bool {
		if out.Rollouts[i].Tenant != out.Rollouts[j].Tenant {
			return out.Rollouts[i].Tenant < out.Rollouts[j].Tenant
		}
		return out.Rollouts[i].Profile < out.Rollouts[j].Profile
	})
	return out, nil
}

// registerRolloutRoutes serves the rollouts, for whoever may audit the
// cluster.
func (h *HTTPServer) registerRolloutRoutes(mux router) {
	mux.Read("GET /v1/rollouts", h.handleRollouts)
}

func (h *HTTPServer) handleRollouts(w http.ResponseWriter, r *http.Request) {
	res, err := h.Service.ListRollouts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
