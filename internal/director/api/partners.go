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
	"net/http"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/guard"
)

// A tenant's partners (Tenant spec.partners, AD-17): realms of other
// clusters its realm links its people's accounts with.
//
// Two kinds of caller write them.
//
// The tenant's administrators (can_administer on it), within the issuers the
// Cluster claim allows.
//
// A component that commissions partners -- a service that links every
// customer cluster that registers with it to its own tenant's realm. It
// presents the key the operator gave it, and the operator lists that key for
// a tenant only where both its profile declares requires.services.partners.
// commission and the tenant names it in spec.partnerCommissioners
// (internal/controller/partner_commissioners.go). It writes receiving
// entries only, and never holds a credential of the realm.
//
// A key opens these routes and nothing else. No other route looks for one,
// and a key is not a token, so everywhere else it is a caller nobody can
// identify.

// commissionerPrincipal prefixes the principal of a call made with a
// component's key.
const commissionerPrincipal = "component:"

// commissioned registers a route about one tenant's partners, for a
// component's key or a person allowed relation on obj.
func (s *Server) commissioned(pattern, relation string, obj object, h func(http.ResponseWriter, *http.Request, call)) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if holder := s.cfg.CommissionKeys.holder(guard.Bearer(r)); holder != nil {
			if c, ok := s.admitCommissioner(w, r, pattern, holder); ok {
				h(w, r, c)
			}
			return
		}
		if c, ok := s.authorize(w, r, pattern, relation, obj); ok {
			h(w, r, c)
		}
	})
}

// admitCommissioner holds a component's key to the tenants the operator
// wrote beside it.
func (s *Server) admitCommissioner(w http.ResponseWriter, r *http.Request, pattern string, holder *CommissionKey) (call, bool) {
	ctx := r.Context()
	tenant := r.PathValue("t")
	if !gentianov1alpha1.ValidTenantName(tenant) {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return call{}, false
	}
	if !holder.may(tenant) {
		s.cfg.Log.WarnContext(ctx, "refused: a component's key was used for a tenant that does not name it",
			"request_id", guard.ID(ctx), "route", pattern, "key_tenant", holder.Tenant, "key_component", holder.Component,
			"tenant", tenant)
		s.fail(w, r, http.StatusForbidden,
			"forbidden: this component may commission partners only for the tenants that name it in spec.partnerCommissioners")
		return call{}, false
	}
	return call{meta: gitops.Meta{
		Author:    gitops.Person{Name: holder.Component + " (tenant " + holder.Tenant + ")"},
		Principal: commissionerPrincipal + holder.Tenant + "/" + holder.Component,
		RequestID: guard.ID(ctx),
		Decision:  "commissions_partners " + authz.Tenant(tenant),
	}}, true
}

// tenantPartners answers a tenant's partners.
func (s *Server) tenantPartners(w http.ResponseWriter, r *http.Request, _ call) {
	list, err := s.cfg.Repo.TenantPartners(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if list == nil {
		list = []gitops.Partner{}
	}
	s.json(w, http.StatusOK, map[string]any{"partners": list})
}

// setTenantPartner adds a partner, or replaces the one of the name in the
// path.
func (s *Server) setTenantPartner(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Issuer string                `json:"issuer"`
		As     string                `json:"as"`
		Issue  bool                  `json:"issue"`
		Accept *gitops.PartnerAccept `json:"accept"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest,
			`body must be {"issuer": "https://<host>/<path to the realm>", "issue": true, "accept": {"accounts": "link|create|organization", "domain": "<organization only>"}}, with "as" optional`)
		return
	}
	res, err := s.cfg.Repo.SetTenantPartner(r.Context(), r.PathValue("t"), gitops.Partner{
		Name: r.PathValue("name"), Issuer: body.Issuer, As: body.As, Issue: body.Issue, Accept: body.Accept,
	}, strings.HasPrefix(c.meta.Principal, commissionerPrincipal), c.meta)
	s.partnerWritten(w, r, res, err)
}

// removeTenantPartner takes a partner away.
func (s *Server) removeTenantPartner(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RemoveTenantPartner(r.Context(), r.PathValue("t"), r.PathValue("name"),
		strings.HasPrefix(c.meta.Principal, commissionerPrincipal), c.meta)
	s.partnerWritten(w, r, res, err)
}

func (s *Server) partnerWritten(w http.ResponseWriter, r *http.Request, res gitops.Result, err error) {
	switch {
	case errors.Is(err, gitops.ErrInvalidPartner):
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, gitops.ErrPartnerNotAllowed), errors.Is(err, gitops.ErrPartnerHeld):
		s.fail(w, r, http.StatusForbidden, err.Error())
	default:
		s.written(w, r, res, err)
	}
}
