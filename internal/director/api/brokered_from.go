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

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// A tenant's outside realms (Tenant spec.brokeredFrom): realms of other
// clusters whose people sign in to the tenant, each as an organization.
//
// Two kinds of caller write them.
//
// An administrator of the cluster (can_configure on it): an outside realm's
// people signing in is a decision about the whole cluster's trust, not the
// tenant's alone.
//
// A component that commissions organizations -- a shop that sells clusters
// and links each one's people to the shop's own tenant once it is made. It
// presents the key the operator gave it, and the operator lists that key for
// a tenant only where both its profile declares requires.services.
// organizations and the tenant names it in spec.organizationCommissioners
// (internal/controller/organizations.go). The component commissions: it
// writes the declaration, the director commits it, and the composition makes
// the organization and the identity provider. It never holds a credential
// of the realm.
//
// A key opens these routes and nothing else. No other route looks for one,
// and a key is not a token, so everywhere else it is a caller nobody can
// identify.

// commissioned registers a route about one tenant's outside realms, for a
// component's key or a person allowed relation on obj.
func (s *Server) commissioned(pattern, relation string, obj object, h func(http.ResponseWriter, *http.Request, call)) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if holder := s.cfg.CommissionKeys.holder(bearerToken(r)); holder != nil {
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
	if !gitops.ValidName(tenant) {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return call{}, false
	}
	if !holder.may(tenant) {
		s.cfg.Log.WarnContext(ctx, "refused: a component's key was used for a tenant that does not name it",
			"request_id", reqID(ctx), "route", pattern, "key_tenant", holder.Tenant, "key_component", holder.Component,
			"tenant", tenant)
		s.fail(w, r, http.StatusForbidden,
			"forbidden: this component may commission organizations only for the tenants that name it in spec.organizationCommissioners")
		return call{}, false
	}
	principal := "component:" + holder.Tenant + "/" + holder.Component
	return call{meta: gitops.Meta{
		Author:    gitops.Person{Name: holder.Component + " (tenant " + holder.Tenant + ")"},
		Principal: principal,
		RequestID: reqID(ctx),
		Decision:  "commissions_organizations " + authz.Tenant(tenant),
	}}, true
}

// bearerToken is the request's bearer credential, or empty.
func bearerToken(r *http.Request) string {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// thisCluster is the object a cluster's administrators are checked against,
// for a route whose path names a tenant and no cluster.
func (s *Server) thisCluster(*http.Request) (string, error) {
	return authz.Cluster(s.cfg.Cluster), nil
}

// tenantBrokeredFrom answers a tenant's outside realms.
func (s *Server) tenantBrokeredFrom(w http.ResponseWriter, r *http.Request, _ call) {
	list, err := s.cfg.Repo.TenantBrokeredFrom(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if list == nil {
		list = []gitops.BrokeredFrom{}
	}
	s.json(w, http.StatusOK, map[string]any{"brokeredFrom": list})
}

// setTenantBrokeredFrom adds an outside realm, or replaces the one of the
// name in the path.
func (s *Server) setTenantBrokeredFrom(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		DisplayName string `json:"displayName"`
		Issuer      string `json:"issuer"`
		ClientID    string `json:"clientID"`
		Domain      string `json:"domain"`
		// LinkExisting: no organization and no domain; see gitops.BrokeredFrom.
		LinkExisting bool `json:"linkExisting"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest,
			`body must be {"displayName": "...", "issuer": "https://id.<cluster>/auth/realms/<realm>", "clientID": "...", "domain": "<domain>"}, or with "linkExisting": true and no domain`)
		return
	}
	res, err := s.cfg.Repo.SetTenantBrokeredFrom(r.Context(), r.PathValue("t"), gitops.BrokeredFrom{
		Name: r.PathValue("name"), DisplayName: body.DisplayName, Issuer: body.Issuer,
		ClientID: body.ClientID, Domain: body.Domain, LinkExisting: body.LinkExisting,
	}, c.meta)
	switch {
	case errors.Is(err, gitops.ErrInvalidBrokeredFrom):
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, gitops.ErrBrokeredFromHeld):
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// removeTenantBrokeredFrom takes an outside realm away.
func (s *Server) removeTenantBrokeredFrom(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RemoveTenantBrokeredFrom(r.Context(), r.PathValue("t"), r.PathValue("name"), c.meta)
	if errors.Is(err, gitops.ErrInvalidBrokeredFrom) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}
