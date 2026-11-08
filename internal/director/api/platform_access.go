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
	"strings"
	"time"
	"unicode"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/guard"
)

// Who on the cluster holds which role by name (spec.platformAccess.roleHolders
// on the Cluster claim), declared by the cluster's administrator, and whether
// a tenant lets the platform's administrators sign in to it, declared on its
// manifest by its own.
//
// Both are commits. The director writes nothing to the authorization graph
// here: the operator projects the claim's holders into it.

// roleHolders answers who the claim names, each with whether it is in
// effect: an entry whose expiresAt has passed, or whose person of a tenant
// other than the platform's is no longer one of its administrators, stays
// in the claim -- it is the record -- and grants nothing, as the operator
// projects it.
func (s *Server) roleHolders(w http.ResponseWriter, r *http.Request, _ call) {
	holders, err := s.cfg.Repo.RoleHolders(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	type entry struct {
		gitops.RoleHolder
		// NotInEffect says why the entry grants nothing; empty while it does.
		NotInEffect string `json:"notInEffect,omitempty"`
	}
	out := make([]entry, 0, len(holders))
	for _, h := range holders {
		e := entry{RoleHolder: h}
		if at, err := time.Parse(time.RFC3339, h.ExpiresAt); err == nil && !at.After(time.Now()) {
			e.NotInEffect = "expired"
		} else if h.Tenant != authz.PlatformTenant {
			admin, err := s.tenantAdministrator(r, h.Tenant, h.Subject)
			if err != nil {
				s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
				return
			}
			if !admin {
				e.NotInEffect = "no longer an administrator of " + h.Tenant
			}
		}
		out = append(out, e)
	}
	review, err := s.cfg.Repo.LastAccessReview(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": r.PathValue("c"), "roles": gitops.HolderRoleNames(), "roleHolders": out,
		"accessReview": review, "reviewDue": review.Due(time.Now())})
}

// reviewedMembers are the registrar's answer for the groups behind admin
// and breakGlass, as the reviewer was shown it.
type reviewedMembers struct {
	Role    string `json:"role"`
	Group   string `json:"group"`
	Members []struct {
		Username string `json:"username"`
		Email    string `json:"email"`
	} `json:"members"`
	Unavailable bool `json:"unavailable"`
}

// defaultReviewInterval is how long until the next review when the reviewer
// names no date: a quarter.
const defaultReviewInterval = 90 * 24 * time.Hour

// reviewAccess records that the caller reviewed who holds a role over the
// cluster: the holders the claim names, read here, and the members of the
// role groups the caller was shown, from the registrar. One commit, whose
// body lists both.
func (s *Server) reviewAccess(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Members      []reviewedMembers `json:"members"`
		NextReviewAt string            `json:"nextReviewAt"`
	}
	usage := `body must be {"members": [{"role", "group", "members": [{"username", "email"}]}], "nextReviewAt": "<RFC 3339, optional>"}: ` +
		`the members as the registrar's platform-role-members answered`
	if err := decode(r, &body); err != nil || body.Members == nil || len(body.Members) > 16 {
		s.fail(w, r, http.StatusBadRequest, usage)
		return
	}
	at := time.Now()
	next := at.Add(defaultReviewInterval)
	if body.NextReviewAt != "" {
		t, err := time.Parse(time.RFC3339, body.NextReviewAt)
		if err != nil || !t.After(at) || t.After(at.Add(366*24*time.Hour)) {
			s.fail(w, r, http.StatusBadRequest, "nextReviewAt must be an RFC 3339 time within the coming year")
			return
		}
		next = t
	}
	holders, err := s.cfg.Repo.RoleHolders(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	lines := []string{}
	for _, g := range body.Members {
		if len(g.Members) > 256 {
			s.fail(w, r, http.StatusBadRequest, usage)
			return
		}
		who := []string{}
		for _, m := range g.Members {
			who = append(who, reviewText(m.Username, m.Email))
		}
		switch {
		case g.Unavailable:
			who = []string{"(could not be read)"}
		case len(who) == 0:
			who = []string{"(nobody)"}
		}
		lines = append(lines, fmt.Sprintf("  - %s through %s: %s", reviewText(g.Role, ""), reviewText(g.Group, ""), strings.Join(who, ", ")))
	}
	for _, h := range holders {
		until := ""
		if h.ExpiresAt != "" {
			until = " until " + h.ExpiresAt
		}
		lines = append(lines, fmt.Sprintf("  - %s over %s: %s of %s%s", h.Role, h.Scope, reviewText(h.Username, h.Email), h.Tenant, until))
	}
	res, err := s.cfg.Repo.RecordAccessReview(r.Context(), next, lines, c.meta)
	s.written(w, r, res, err)
}

// reviewText is a name as one line of a commit body: control characters,
// which could start a line of their own, are dropped, and it is cut short.
func reviewText(name, email string) string {
	clean := func(v string) string {
		v = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, v)
		if len(v) > 255 {
			v = v[:255]
		}
		return v
	}
	if email == "" || strings.EqualFold(name, email) {
		return clean(name)
	}
	return clean(name) + " <" + clean(email) + ">"
}

// setRoleHolder gives a person, by their subject in their tenant's realm, a
// role. The body says who it is to a person reading the claim, why, and
// until when. A person of a tenant other than the platform's has to be one
// of its administrators, as the store holds its admins group's members.
func (s *Server) setRoleHolder(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Scope     string `json:"scope"`
		Username  string `json:"username"`
		Email     string `json:"email"`
		Reason    string `json:"reason"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := decode(r, &body); err != nil || body.Username == "" || body.Reason == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"username": "<the login in the tenant's realm>", "email": "<address, if any>", `+
			`"reason": "<why>", "expiresAt": "<RFC 3339, if it ends>"}`)
		return
	}
	h := gitops.RoleHolder{
		Role: r.PathValue("role"), Scope: body.Scope, Tenant: r.PathValue("tenant"), Subject: r.PathValue("subject"),
		Username: body.Username, Email: body.Email, Reason: body.Reason, ExpiresAt: body.ExpiresAt,
	}
	if h.Tenant != authz.PlatformTenant {
		admin, err := s.tenantAdministrator(r, h.Tenant, h.Subject)
		if err != nil {
			s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		if !admin {
			s.fail(w, r, http.StatusUnprocessableEntity, "only an administrator of "+h.Tenant+" can be named from that tenant")
			return
		}
	}
	res, err := s.cfg.Repo.SetRoleHolder(r.Context(), h, c.meta)
	if errors.Is(err, gitops.ErrInvalidRoleHolder) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// tenantAdministrator says whether the account subject is an administrator
// of tenant (authz.TenantAdministrator). A tenant that is not a name is not
// asked about.
func (s *Server) tenantAdministrator(r *http.Request, tenant, subject string) (bool, error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return false, nil
	}
	return authz.TenantAdministrator(r.Context(), s.cfg.Authz, guard.ID(r.Context()), tenant, subject)
}

func (s *Server) removeRoleHolder(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RemoveRoleHolder(r.Context(), r.PathValue("role"), r.PathValue("tenant"), r.PathValue("subject"), c.meta)
	if errors.Is(err, gitops.ErrInvalidRoleHolder) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// tenantPlatformAccess answers whether the platform's administrators may
// sign in to the tenant.
func (s *Server) tenantPlatformAccess(w http.ResponseWriter, r *http.Request, _ call) {
	on, err := s.cfg.Repo.TenantPlatformAccess(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "adminsEnter": on})
}

// setTenantPlatformAccess writes the switch.
func (s *Server) setTenantPlatformAccess(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		AdminsEnter *bool `json:"adminsEnter"`
	}
	if err := decode(r, &body); err != nil || body.AdminsEnter == nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"adminsEnter": true} or {"adminsEnter": false}`)
		return
	}
	res, err := s.cfg.Repo.SetTenantPlatformAccess(r.Context(), r.PathValue("t"), *body.AdminsEnter, c.meta)
	if errors.Is(err, gitops.ErrPlatformAccessPlatformTenant) {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	s.written(w, r, res, err)
}
