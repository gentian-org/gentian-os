/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// The cluster's resource plans: reading them, and changing them.
//
// The plans are files in git (gitops/resource_plans.go), written here for
// whoever may configure the cluster. Two rules need to know which tenant is
// on which plan, which is the operator's answer, not git's: a plan's
// quantities do not change under the tenants on it, because each of them
// holds a copy in its resource-plan.yaml and would be left on a ceiling that
// is no longer the plan's; and a plan a tenant is on is not removed. When the
// operator cannot be asked, a change those rules decide is refused rather
// than made blind.

// planTenants is the operator's answer to who is on which plan: the plan a
// tenant's quotas match, and the plan it was last set to, which differ when
// it drifted. Both count.
func (s *Server) planTenants(ctx context.Context) (map[string][]string, error) {
	if s.cfg.Lifecycle == nil {
		return nil, errors.New("no app-lifecycle API is configured")
	}
	status, body, err := s.cfg.Lifecycle.Get(ctx, "/v1/resources", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("app-lifecycle API answered %d", status)
	}
	var answer struct {
		Tenants []struct {
			Tenant        string `json:"tenant"`
			Plan          string `json:"plan"`
			AnnotatedPlan string `json:"annotatedPlan"`
		} `json:"tenants"`
		Unavailable []struct {
			Tenant string `json:"tenant"`
		} `json:"unavailable"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("app-lifecycle API: resources: %w", err)
	}
	// A tenant whose state the operator could not work out may be on any
	// plan; not knowing is not the same as nobody.
	if len(answer.Unavailable) > 0 {
		return nil, fmt.Errorf("the operator could not tell the plan of tenant %s", answer.Unavailable[0].Tenant)
	}
	out := map[string][]string{}
	for _, t := range answer.Tenants {
		seen := map[string]bool{}
		for _, p := range []string{t.Plan, t.AnnotatedPlan} {
			if p != "" && !seen[p] {
				seen[p] = true
				out[p] = append(out[p], t.Tenant)
			}
		}
	}
	for p := range out {
		sort.Strings(out[p])
	}
	return out, nil
}

type planWithTenants struct {
	gitops.PlanDefinition
	// Tenants are the tenants on the plan, absent when the operator could not
	// be asked (TenantsKnown false on the answer).
	Tenants []string `json:"tenants,omitempty"`
}

// clusterResourcePlans lists the plans git holds, each with the tenants on it.
func (s *Server) clusterResourcePlans(w http.ResponseWriter, r *http.Request, _ call) {
	defs, err := s.cfg.Repo.PlanDefinitions(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	tenants, terr := s.planTenants(r.Context())
	out := make([]planWithTenants, 0, len(defs))
	for _, d := range defs {
		p := planWithTenants{PlanDefinition: d}
		if terr == nil {
			p.Tenants = tenants[d.Name]
		}
		out = append(out, p)
	}
	s.json(w, http.StatusOK, map[string]any{
		"cluster":      r.PathValue("c"),
		"plans":        out,
		"tenantsKnown": terr == nil,
	})
}

// setClusterResourcePlan adds a plan or changes one.
func (s *Server) setClusterResourcePlan(w http.ResponseWriter, r *http.Request, c call) {
	var body gitops.PlanDefinition
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, "body must be a resource plan")
		return
	}
	body.Name = r.PathValue("name")
	if err := gitops.CheckPlanDefinition(&body); err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	defs, err := s.cfg.Repo.PlanDefinitions(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	var existing *gitops.PlanDefinition
	var currentDefault *gitops.PlanDefinition
	for i := range defs {
		d := &defs[i]
		if d.Name == body.Name {
			existing = d
			continue
		}
		if d.Tier == body.Tier {
			s.fail(w, r, http.StatusConflict, fmt.Sprintf("tier %d is plan %s's; every plan has a tier of its own", body.Tier, d.Name))
			return
		}
		if d.Default {
			currentDefault = d
		}
	}
	if existing != nil && existing.Default && !body.Default {
		s.fail(w, r, http.StatusConflict, "this is the cluster's default plan; make another plan the default instead")
		return
	}
	if existing == nil && len(defs) == 0 && !body.Default {
		s.fail(w, r, http.StatusConflict, "the cluster's first plan is its default")
		return
	}
	if existing != nil && !gitops.SameQuotas(existing.Quotas, body.Quotas) {
		tenants, err := s.planTenants(r.Context())
		if err != nil {
			s.cfg.Log.WarnContext(r.Context(), "plan change refused: who is on it is unknown",
				"request_id", reqID(r.Context()), "plan", body.Name, "error", err.Error())
			s.fail(w, r, http.StatusServiceUnavailable, "which tenants are on this plan could not be read from the operator; its quotas are not changed blind")
			return
		}
		if on := tenants[body.Name]; len(on) > 0 {
			s.fail(w, r, http.StatusConflict, fmt.Sprintf(
				"%s is on this plan, so its quotas cannot change: make a new plan with these quotas and move the tenants to it", namesOf(on)))
			return
		}
	}

	writes := []gitops.PlanWrite{{Plan: body}}
	verb := "Add"
	if existing != nil {
		verb = "Change"
	}
	message := fmt.Sprintf("%s resource plan %s", verb, body.Name)
	if body.Default && currentDefault != nil {
		moved := *currentDefault
		moved.Default = false
		writes = append(writes, gitops.PlanWrite{Plan: moved})
		message += fmt.Sprintf(", the default in place of %s", currentDefault.Name)
	}
	res, err := s.cfg.Repo.WritePlans(r.Context(), message, c.meta, writes...)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	answer := map[string]any{"status": res.Status, "plan": body.Name}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	s.json(w, http.StatusAccepted, answer)
}

// removeClusterResourcePlan removes a plan nobody is on.
func (s *Server) removeClusterResourcePlan(w http.ResponseWriter, r *http.Request, c call) {
	name := r.PathValue("name")
	defs, err := s.cfg.Repo.PlanDefinitions(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	var existing *gitops.PlanDefinition
	for i := range defs {
		if defs[i].Name == name {
			existing = &defs[i]
		}
	}
	if existing == nil {
		s.fail(w, r, http.StatusNotFound, gitops.ErrPlanNotFound.Error())
		return
	}
	if existing.Default {
		s.fail(w, r, http.StatusConflict, "this is the cluster's default plan; make another plan the default first")
		return
	}
	tenants, err := s.planTenants(r.Context())
	if err != nil {
		s.cfg.Log.WarnContext(r.Context(), "plan removal refused: who is on it is unknown",
			"request_id", reqID(r.Context()), "plan", name, "error", err.Error())
		s.fail(w, r, http.StatusServiceUnavailable, "which tenants are on this plan could not be read from the operator; it is not removed blind")
		return
	}
	if on := tenants[name]; len(on) > 0 {
		s.fail(w, r, http.StatusConflict, fmt.Sprintf("%s is on this plan; retire it instead, or move them first", namesOf(on)))
		return
	}
	res, err := s.cfg.Repo.WritePlans(r.Context(), fmt.Sprintf("Remove resource plan %s", name), c.meta,
		gitops.PlanWrite{Plan: *existing, Remove: true})
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusAccepted, map[string]any{"status": "removed", "plan": name, "commit": res.Commit})
}

// namesOf says who, briefly: a long list is cut after three.
func namesOf(tenants []string) string {
	if len(tenants) == 1 {
		return "tenant " + tenants[0]
	}
	if len(tenants) <= 3 {
		return "tenants " + strings.Join(tenants, ", ")
	}
	return fmt.Sprintf("tenants %s and %d more", strings.Join(tenants[:3], ", "), len(tenants)-3)
}

// clusterPlanOrdering reads the notice: {"set": false} when the cluster
// states none, and tenants are told to ask its administrator.
func (s *Server) clusterPlanOrdering(w http.ResponseWriter, r *http.Request, _ call) {
	spec, found, err := s.cfg.Repo.PlanOrdering(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if !found {
		s.json(w, http.StatusOK, map[string]any{"set": false})
		return
	}
	s.json(w, http.StatusOK, map[string]any{"set": true, "ordering": spec})
}

func (s *Server) setClusterPlanOrdering(w http.ResponseWriter, r *http.Request, c call) {
	var body gentianov1alpha1.PlanOrderingSpec
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"text": {"en": "..."}, "url" | "email" | "phone": "..."}`)
		return
	}
	res, err := s.cfg.Repo.SetPlanOrdering(r.Context(), body, c.meta)
	s.written(w, r, res, err)
}

func (s *Server) clearClusterPlanOrdering(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.ClearPlanOrdering(r.Context(), c.meta)
	s.written(w, r, res, err)
}
