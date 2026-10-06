/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package usher tells a signed-in person what is here and what they may open
// (operator-split-plan.md §3.10).
//
// It decides nothing and changes nothing. Every answer is a projection the
// operator wrote, filtered by a question put to the authorization store with
// the caller's own identity. It holds no git credential, no signing key and
// no Kubernetes access, which is why these reads are here and not in the
// director: a tenant's own people need them, and the process that answers
// them should have nothing worth taking.
//
// Its routes are lists, and a list is where one tenant's objects leak to
// another. So a route is registered only through guarded, which names the
// object and the relation before the handler runs; a route that filtered in
// its handler instead is the mistake to refuse in review.
package usher

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

// Authenticator says who presented a request.
type Authenticator interface {
	FromRequest(r *http.Request) (*authn.Identity, error)
}

// Config is what a Server needs.
type Config struct {
	Authn Authenticator
	Authz authz.Checker
	// TilesPath is the file the operator's tile catalogue is mounted at.
	TilesPath string
	Log       *slog.Logger
}

// Server is the usher's HTTP surface.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New builds a Server with its routes registered.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Whoever may enter a tenant may be told what is in it. Each tile is then
	// asked about on its own, so entering shows a person their tiles and not
	// the tenant's.
	s.guarded("GET /v1/tenants/{t}/tiles", "can_enter", s.tenantTiles)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// tenantName is a DNS label, which is what a tenant's name is. Checked here
// because the name becomes part of an object id put to the store.
var tenantName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,50}[a-z0-9])?$`)

// call is a request that has passed the guard.
type call struct {
	user   string
	tenant string
}

// guarded registers a route about one tenant with the relation it requires on
// that tenant. There is no other way to register a route.
func (s *Server) guarded(pattern, relation string, h func(http.ResponseWriter, *http.Request, call)) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ident, err := s.cfg.Authn.FromRequest(r)
		if err != nil {
			s.cfg.Log.WarnContext(ctx, "authentication failed", "reason", err.Error(), "route", pattern)
			w.Header().Set("WWW-Authenticate", `Bearer realm="gentian-usher"`)
			fail(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		user, err := authz.User(ident.Subject)
		if err != nil {
			fail(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		tenant := r.PathValue("t")
		if !tenantName.MatchString(tenant) {
			fail(w, http.StatusBadRequest, "invalid name")
			return
		}
		allowed, err := s.cfg.Authz.Check(ctx, "", user, relation, authz.Tenant(tenant))
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		if !allowed {
			fail(w, http.StatusForbidden, "forbidden")
			return
		}
		h(w, r, call{user: user, tenant: tenant})
	})
}

type tileOut struct {
	Name         string            `json:"name"`
	DisplayName  string            `json:"displayName"`
	DisplayNames map[string]string `json:"displayNames,omitempty"`
	Description  string            `json:"description"`
	URL          string            `json:"url"`
	Icon         string            `json:"icon"`
}

// catalogue is what the operator projected.
//
// A catalogue that cannot be read is a failure and is answered as one. An
// empty list in its place would show a person a desktop with nothing on it,
// which looks exactly like holding no rights and says nothing about the
// projection being missing.
func (s *Server) catalogue() ([]tilecatalogue.Tile, error) {
	data, err := os.ReadFile(s.cfg.TilesPath)
	if err != nil {
		return nil, err
	}
	c, err := tilecatalogue.Parse(data)
	if err != nil {
		return nil, err
	}
	return c.Tiles, nil
}

// inTenant reports whether a tile belongs on this tenant's desktop.
//
// A tile about another tenant, or about another tenant's app, is never this
// tenant's, whatever the caller holds there: it sits behind that tenant's own
// session. A tile about the cluster is the kernel's own console, shown to
// whoever holds its relation.
func inTenant(object, tenant string) bool {
	switch {
	case strings.HasPrefix(object, "tenant:"):
		return object == "tenant:"+tenant
	case strings.HasPrefix(object, "app:"):
		return strings.HasPrefix(object, "app:"+tenant+"/")
	case strings.HasPrefix(object, "cluster:"):
		return true
	}
	return false
}

// tenantTiles answers with the tiles of one tenant this caller may open. A
// tile the caller may not open is left out, because showing it and refusing
// the click tells a person about something they have no business knowing of.
func (s *Server) tenantTiles(w http.ResponseWriter, r *http.Request, c call) {
	ctx := r.Context()
	catalogue, err := s.catalogue()
	if err != nil {
		s.cfg.Log.ErrorContext(ctx, "the tile catalogue cannot be read", "path", s.cfg.TilesPath, "error", err.Error())
		fail(w, http.StatusServiceUnavailable, "the tile catalogue cannot be read")
		return
	}
	out := []tileOut{}
	for _, t := range catalogue {
		if !inTenant(t.Object, c.tenant) {
			continue
		}
		for _, rel := range t.AnyOf {
			ok, err := s.cfg.Authz.Check(ctx, "", c.user, rel, t.Object)
			if err != nil {
				fail(w, http.StatusServiceUnavailable, "authorization unavailable")
				return
			}
			if ok {
				out = append(out, tileOut{
					Name: t.Name, DisplayName: t.DisplayName, DisplayNames: t.DisplayNames,
					Description: t.Description, URL: t.URL, Icon: t.Icon,
				})
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": c.tenant, "tiles": out})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}
