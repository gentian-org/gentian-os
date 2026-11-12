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
	"fmt"
	"net/http"
	"slices"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// clusterModels answers the model settings of the Cluster claim, and the
// models they make the gateway offer, each with what the claim alone says
// about whether it works.
//
// From git, like every read of this service: the gateway is not asked, and
// neither is the vault. So an instance's model is reported as not served
// because the platform starts no instance, and a provider's model as
// declared, with the name of the credential its token belongs under -- whether
// that credential is there is the custodian's to say.
func (s *Server) clusterModels(w http.ResponseWriter, r *http.Request, _ call) {
	settings, err := s.cfg.Repo.ClusterModels(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{
		"cluster":  s.cfg.Cluster,
		"settings": settings,
		"models":   settings.GatewayModels(),
	})
}

// setClusterModels commits the model settings: the whole of them, so a model
// the body does not name is removed from the claim and with it from the
// gateway.
func (s *Server) setClusterModels(w http.ResponseWriter, r *http.Request, c call) {
	var want gitops.ModelSettings
	if err := decode(r, &want); err != nil {
		s.fail(w, r, http.StatusBadRequest,
			"body must be the model settings: enabled, gpuAcceleration, console, instances, providers -- and no provider's token, which is a credential")
		return
	}
	if !s.providerAddressesResolvePublicly(w, r, want) {
		return
	}
	res, err := s.cfg.Repo.SetClusterModels(r.Context(), want, c.meta)
	switch {
	case errors.Is(err, gitops.ErrInvalidModels):
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, gitops.ErrModelsFlowForm):
		s.fail(w, r, http.StatusConflict, gitops.ErrModelsFlowForm.Error())
	default:
		if err == nil && res.Changed {
			// Switching models on or off places or removes the apps that
			// come with them.
			s.placePlatformAppsNow()
		}
		s.written(w, r, res, err)
	}
}

// providerAddressesResolvePublicly is the half of the address rule that needs
// the network: the host of a provider's address resolves, and to public
// addresses only. It is asked of an address the claim does not carry yet --
// one already there was asked when it was added, and a provider whose name
// stopped resolving must not stand in the way of changing another. What is
// wrong with an address as it is written is not looked at here: the write
// refuses that, for every provider (gitops.ValidateModelSettings).
//
// The gateway, not the director, connects to a provider, and it resolves the
// name again each time: this is a check of what was typed, when it was typed.
func (s *Server) providerAddressesResolvePublicly(w http.ResponseWriter, r *http.Request, want gitops.ModelSettings) bool {
	have, err := s.cfg.Repo.ClusterModels(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return false
	}
	known := map[string]bool{}
	for _, p := range have.Providers {
		known[p.APIBase] = true
	}
	for i, p := range want.Providers {
		if known[p.APIBase] || catalogue.CheckAddress(p.APIBase) != nil {
			continue
		}
		if s.cfg.Catalogue == nil || s.cfg.Catalogue.Vet == nil {
			s.fail(w, r, http.StatusServiceUnavailable, "this director cannot check an address, so no provider's address can be set or changed")
			return false
		}
		if err := s.cfg.Catalogue.Vet(r.Context(), p.APIBase); err != nil {
			if errors.Is(err, catalogue.ErrAddressRefused) {
				s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf("%s: spec.llm.providers[%d].apiBase is refused: %s",
					gitops.ErrInvalidModels, i, gitops.AddressRefusal(err)))
				return false
			}
			s.repoError(w, r, err)
			return false
		}
	}
	return true
}

// tenantModelAccess answers what a tenant's apps may use at the model
// gateway: what its manifest says (nothing is the models every tenant may
// use, without a cap), and each model of the cluster with whether the
// tenant's apps may use it.
func (s *Server) tenantModelAccess(w http.ResponseWriter, r *http.Request, _ call) {
	tenant := r.PathValue("t")
	access, err := s.cfg.Repo.TenantModelAccess(r.Context(), tenant)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	settings, err := s.cfg.Repo.ClusterModels(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if access == nil {
		access = &gentianov1alpha1.TenantLLM{}
	}
	type model struct {
		gitops.GatewayModel
		// Allowed says the tenant's apps may use the model.
		Allowed bool `json:"allowed"`
	}
	models := []model{}
	for _, m := range settings.GatewayModels() {
		allowed := m.Kind == "instance" || slices.Contains(access.Providers, m.Source)
		models = append(models, model{GatewayModel: m, Allowed: allowed})
	}
	providers := access.Providers
	if providers == nil {
		providers = []string{}
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "providers": providers, "budget": access.Budget, "delegated": access.Delegated, "models": models,
	})
}

// setTenantModelAccess commits what a tenant's apps may use at the model
// gateway: the providers it opts in to, its budget and whether its own
// administrators may change the providers, the whole of it. An empty body
// object is the models every tenant may use, without a cap.
//
// Whoever the cluster's relation did not let in came in as the tenant's
// administrator: the write then holds them to the providers, and only while
// they are delegated (gitops.SetTenantModelAccess).
func (s *Server) setTenantModelAccess(w http.ResponseWriter, r *http.Request, c call) {
	var body gentianov1alpha1.TenantLLM
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest,
			`body must be {"providers": ["<provider>", ...], "budget": {"maxBudget": <whole US dollars>, "budgetDuration": "30d", "rpmLimit": <n>, "tpmLimit": <n>}, "delegated": <bool>}, each part optional`)
		return
	}
	by := gitops.ModelAccessByCluster
	if c.held != "can_configure "+authz.Cluster(s.cfg.Cluster) {
		by = gitops.ModelAccessByTenant
	}
	res, err := s.cfg.Repo.SetTenantModelAccess(r.Context(), r.PathValue("t"), &body, by, c.meta)
	switch {
	case errors.Is(err, gitops.ErrInvalidModelAccess):
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, gitops.ErrModelAccessNotDelegated):
		s.fail(w, r, http.StatusForbidden, "changing the providers is not delegated to this tenant's administrators; "+
			"the cluster's administrator sets spec.llm.delegated")
	case errors.Is(err, gitops.ErrModelAccessClusterOnly):
		s.fail(w, r, http.StatusForbidden, "the budget and spec.llm.delegated are the cluster's administrator's; "+
			"send them as they are and change only the providers")
	default:
		s.written(w, r, res, err)
	}
}
