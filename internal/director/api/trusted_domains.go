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

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// A tenant's own trusted domains (Tenant spec.trustedDomains): an egress
// request of one of the tenant's apps whose hosts are all in trusted domains
// is granted without a further answer.
//
//	GET    /v1/tenants/{t}/trusted-domains             the list and whether it is delegated
//	PUT    /v1/tenants/{t}/trusted-domains/{domain}    trust one more
//	DELETE /v1/tenants/{t}/trusted-domains/{domain}    stop trusting one
//	PUT    /v1/clusters/{c}/tenants/{t}/trusted-domains-delegation
//	DELETE /v1/clusters/{c}/tenants/{t}/trusted-domains-delegation
//
// Adding a domain answers every later request for a host in it, so it is
// whose answer such a request is: the tenant's perimeter approver's
// (tenant#can_approve_privilege), and only while the cluster's administrator
// delegated the list to the tenant (cluster#can_configure).

func (s *Server) tenantTrustedDomains(w http.ResponseWriter, r *http.Request, _ call) {
	tenant := r.PathValue("t")
	have, err := s.cfg.Repo.TenantTrustedDomains(r.Context(), tenant)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	domains := have.Domains
	if domains == nil {
		domains = []string{}
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": tenant, "delegated": have.Delegated, "domains": domains})
}

func (s *Server) addTrustedDomain(w http.ResponseWriter, r *http.Request, c call) {
	tenant, domain := r.PathValue("t"), r.PathValue("domain")
	res, err := s.cfg.Repo.AddTenantTrustedDomain(r.Context(), tenant, domain, c.meta)
	s.trustedDomainWritten(w, r, res, err, map[string]any{"tenant": tenant, "domain": domain})
}

func (s *Server) removeTrustedDomain(w http.ResponseWriter, r *http.Request, c call) {
	tenant, domain := r.PathValue("t"), r.PathValue("domain")
	res, err := s.cfg.Repo.RemoveTenantTrustedDomain(r.Context(), tenant, domain, c.meta)
	s.trustedDomainWritten(w, r, res, err, map[string]any{"tenant": tenant, "domain": domain})
}

func (s *Server) delegateTrustedDomains(w http.ResponseWriter, r *http.Request, c call) {
	s.setTrustedDomainsDelegation(w, r, c, true)
}

func (s *Server) undelegateTrustedDomains(w http.ResponseWriter, r *http.Request, c call) {
	s.setTrustedDomainsDelegation(w, r, c, false)
}

func (s *Server) setTrustedDomainsDelegation(w http.ResponseWriter, r *http.Request, c call, delegated bool) {
	tenant := r.PathValue("t")
	res, err := s.cfg.Repo.SetTenantTrustedDomainsDelegation(r.Context(), tenant, delegated, c.meta)
	s.trustedDomainWritten(w, r, res, err, map[string]any{"tenant": tenant, "delegated": delegated})
}

// trustedDomainWritten answers a write: 202 with the commit when git
// changed, 200 when it already was as asked, and the refusal otherwise.
func (s *Server) trustedDomainWritten(w http.ResponseWriter, r *http.Request, res gitops.Result, err error, answer map[string]any) {
	switch {
	case errors.Is(err, gitops.ErrTrustedDomainInvalid):
		s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
			"%q is not a domain: a domain is written in lower case with at least two labels, as in example.org; nothing was changed", r.PathValue("domain")))
		return
	case errors.Is(err, gitops.ErrTrustedDomainsNotDelegated):
		s.fail(w, r, http.StatusForbidden, "trusted domains are not delegated to this tenant; the cluster's administrator delegates them "+
			"(kubectl gentian tenants delegate-trusted-domains <tenant> on)")
		return
	case errors.Is(err, gitops.ErrTrustedDomainNotFound):
		s.fail(w, r, http.StatusNotFound, "no such trusted domain: "+r.PathValue("domain"))
		return
	case errors.Is(err, gitops.ErrTrustedDomainsFull):
		s.fail(w, r, http.StatusConflict, fmt.Sprintf(
			"the tenant's list holds %d trusted domains, which is as many as it may; remove one first", gitops.MaxTenantTrustedDomains))
		return
	case errors.Is(err, gitops.ErrCatalogueFlowForm):
		s.fail(w, r, http.StatusConflict, "the tenant's trusted domains are written on one line; write them as a block to change them here")
		return
	case err != nil:
		s.repoError(w, r, err)
		return
	}
	answer["status"] = res.Status
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	s.json(w, http.StatusAccepted, answer)
}
