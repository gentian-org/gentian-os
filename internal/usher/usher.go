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
// It changes nothing. Every answer is the operator's -- a projection it
// wrote, or what it says when asked -- given to a caller the authorization
// store allowed, asked with the caller's own identity. It holds no git
// credential, no signing key and no access to the Kubernetes API, which is
// why these reads are here and not in the director: a tenant's own people
// need them, and the process that answers them should have as little worth
// taking as can be arranged.
//
// What it does hold, beside the store's key, is its own ServiceAccount's
// token for the operator's listener, which admits that identity to reads
// only (state.go). With it the usher can read every tenant's live state; the
// guard below is what stands between that and a caller. It can issue no
// command with it.
//
// Its routes are lists, and a list is where one tenant's objects leak to
// another. So a route is registered only through guarded, which names the
// object and the relation before the handler runs; a route that filtered in
// its handler instead is the mistake to refuse in review.
package usher

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
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
	// Cluster is the id of the one cluster this usher serves: the object
	// cluster relations are asked about, and the only {c} a route accepts.
	// Empty leaves the cluster's routes unregistered.
	Cluster string
	// Lifecycle asks the operator what only the cluster knows. Nil leaves
	// the reads of live state unregistered: an usher with no operator to ask
	// has nothing to answer them with.
	Lifecycle Lifecycle
	// LicenceReporting is whether this cluster reports what it runs. The App
	// Store is offered only where it does, and the tiles answer says which.
	LicenceReporting bool
}

// Lifecycle is the operator's listener, as far as the usher may use it: two
// ways to read, and no way to ask for anything to be done.
type Lifecycle interface {
	Get(ctx context.Context, path string, query url.Values) (int, []byte, error)
	Plans(ctx context.Context, tenant string, selfService bool) ([]lifecycle.Plan, error)
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
	s.guarded("GET /v1/tenants/{t}/tiles", "can_enter", tenantObject, s.tenantTiles)
	if cfg.Lifecycle != nil {
		s.stateRoutes()
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// tenantName is a DNS label, which is what a tenant's name is. Checked here
// because the name becomes part of an object id put to the store.
var tenantName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,50}[a-z0-9])?$`)

// call is a request that has passed the guard.
type call struct {
	user string
	// tenant is the tenant a tenant route is about; empty on a cluster route.
	tenant string
}

// object names what a route's relation is asked about.
type object func(s *Server, r *http.Request) (string, error)

// tenantObject is the tenant in the path.
func tenantObject(_ *Server, r *http.Request) (string, error) {
	t := r.PathValue("t")
	if !tenantName.MatchString(t) {
		return "", errInvalidName
	}
	return authz.Tenant(t), nil
}

// clusterObject accepts only this usher's own cluster. Another id is not
// forbidden, it does not exist here.
func clusterObject(s *Server, r *http.Request) (string, error) {
	if c := r.PathValue("c"); c == "" || c != s.cfg.Cluster {
		return "", errInvalidName
	}
	return authz.Cluster(s.cfg.Cluster), nil
}

var errInvalidName = errors.New("invalid name")

// guarded registers a route with the relation it requires on the object it
// is about. There is no other way to register a route.
func (s *Server) guarded(pattern, relation string, obj object, h func(http.ResponseWriter, *http.Request, call)) {
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
		target, err := obj(s, r)
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid name")
			return
		}
		allowed, err := s.cfg.Authz.Check(ctx, "", user, relation, target)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		if !allowed {
			fail(w, http.StatusForbidden, "forbidden")
			return
		}
		h(w, r, call{user: user, tenant: r.PathValue("t")})
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
	writeJSON(w, http.StatusOK, map[string]any{"tenant": c.tenant, "tiles": out, "appStore": s.appStore()})
}

// appStoreReasonNoLicenceReport is why a cluster that does not report offers
// no App Store.
const appStoreReasonNoLicenceReport = "licence-report-disabled"

type appStoreOut struct {
	// Available is whether a desktop may offer the App Store at all.
	Available bool `json:"available"`
	// Reason says why not, for a console to put into words: the App Store
	// needs licence reporting, which is turned off on this cluster.
	Reason string `json:"reason,omitempty"`
}

// appStore answers whether the App Store may be offered on this cluster.
//
// It is not a tile: where the store is comes from the cluster's catalogue
// settings, and whether a person may install is asked when they do. This is
// the one thing in front of both. An app installed through the store is what
// the licence report lists, so a cluster that sends no report does not offer
// the store, and says so here instead of leaving a desktop to show a store
// that is merely absent.
func (s *Server) appStore() appStoreOut {
	if s.cfg.LicenceReporting {
		return appStoreOut{Available: true}
	}
	return appStoreOut{Available: false, Reason: appStoreReasonNoLicenceReport}
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
