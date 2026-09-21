/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package api is the director's HTTP surface: the one way a change reaches
// gentian-deployments.
//
// Every route follows the same four steps and none may skip one: establish who
// is calling from a verified token, name the relation the route requires, ask
// OpenFGA, and only then touch git. A route is registered together with its
// relation, so a handler that forgot to authorise is not something this
// package can express.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Authenticator establishes the caller's identity from a request.
type Authenticator interface {
	FromRequest(r *http.Request) (*authn.Identity, error)
}

// Repository is the git backend.
type Repository interface {
	Install(ctx context.Context, tenant, profile string, meta gitops.Meta) (gitops.Result, error)
	Uninstall(ctx context.Context, tenant, profile string, meta gitops.Meta) (gitops.Result, error)
	SetAddons(ctx context.Context, tenant, profile string, addons []string, meta gitops.Meta) (gitops.Result, error)
	Apps(ctx context.Context, tenant string) ([]gitops.App, error)
}

// Config assembles a Server.
type Config struct {
	Authn Authenticator
	Authz authz.Checker
	Repo  Repository
	Log   *slog.Logger
	// EnforceEntitlements makes an install require
	// catalogue_entry:<coordinate>#can_install for the tenant. It is on unless
	// a deployment turns it off explicitly, which a cluster without a store
	// has to do — and which is then visible as a setting, not as an absence.
	EnforceEntitlements bool
	// Events receives Keycloak's membership events. It authenticates its one
	// caller by signature, not by token: the listener is not a user and holds
	// no identity a token could carry. Nil leaves the endpoint unregistered.
	Events http.Handler
}

// Server is the director's API.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New returns a Server with every route registered.
func New(cfg Config) (*Server, error) {
	if cfg.Authn == nil || cfg.Authz == nil || cfg.Repo == nil {
		return nil, errors.New("api: authenticator, checker and repository are all required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := requestID(r)
	w.Header().Set("X-Request-Id", id)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	s.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID{}, id)))
}

type ctxRequestID struct{}

// inboundID is what a request id from the gateway must look like to be kept.
// It ends up in a commit trailer, so it is matched, not escaped.
var inboundID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// requestID keeps the gateway's id so the three audit logs join on one value
// (security principle 7), and mints one for callers that arrive without.
func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); inboundID.MatchString(id) {
		return id
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func reqID(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID{}).(string)
	return id
}

// call is what a handler receives once the caller is known and allowed.
type call struct {
	id   *authn.Identity
	user string // OpenFGA user
	meta gitops.Meta
}

// object names what a route's relation is checked against.
type object func(r *http.Request) (string, error)

func tenantObject(r *http.Request) (string, error) {
	t := r.PathValue("t")
	if !gitops.ValidName(t) {
		return "", gitops.ErrInvalidName
	}
	return authz.Tenant(t), nil
}

// guarded registers a route with the relation it requires. There is no other
// way to register a route a user can call.
func (s *Server) guarded(pattern, relation string, obj object, h func(http.ResponseWriter, *http.Request, call)) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ident, err := s.cfg.Authn.FromRequest(r)
		if err != nil {
			s.cfg.Log.WarnContext(ctx, "authentication failed", "request_id", reqID(ctx), "reason", err.Error(), "route", pattern)
			w.Header().Set("WWW-Authenticate", `Bearer realm="gentian-director"`)
			s.fail(w, r, http.StatusUnauthorized, "unauthenticated")
			return
		}
		user, err := authz.User(ident.Subject)
		if err != nil {
			s.fail(w, r, http.StatusUnauthorized, "unauthenticated")
			return
		}
		target, err := obj(r)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid name")
			return
		}
		allowed, err := s.cfg.Authz.Check(ctx, reqID(ctx), user, relation, target)
		if err != nil {
			// The decision point is unreachable: refuse, and say it is us.
			s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		if !allowed {
			s.fail(w, r, http.StatusForbidden, "forbidden")
			return
		}
		h(w, r, call{
			id:   ident,
			user: user,
			meta: gitops.Meta{
				Author:    gitops.Person{Name: ident.Name, Email: ident.Email},
				Subject:   user[len("user:"):],
				RequestID: reqID(ctx),
				Decision:  relation + " " + target,
			},
		})
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if s.cfg.Events != nil {
		s.mux.Handle("POST /v1/events/keycloak", s.cfg.Events)
	}

	// Reads are authorised by can_view, never by the write relation: whoever
	// may see a tenant may see what it has installed, whether or not they may
	// change it.
	s.guarded("GET /v1/tenants/{t}/apps", "can_view", tenantObject, s.listApps)
	s.guarded("GET /v1/tenants/{t}/apps/{p}/addons", "can_view", tenantObject, s.getAddons)

	s.guarded("POST /v1/tenants/{t}/apps/{p}", "can_install_app", tenantObject, s.install)
	s.guarded("DELETE /v1/tenants/{t}/apps/{p}", "can_install_app", tenantObject, s.uninstall)
	s.guarded("PUT /v1/tenants/{t}/apps/{p}/addons", "can_install_app", tenantObject, s.setAddons)
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request, _ call) {
	apps, err := s.cfg.Repo.Apps(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "apps": apps})
}

func (s *Server) getAddons(w http.ResponseWriter, r *http.Request, _ call) {
	apps, err := s.cfg.Repo.Apps(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	for _, a := range apps {
		if a.Profile == r.PathValue("p") {
			addons := a.Addons
			if addons == nil {
				addons = []string{}
			}
			s.json(w, http.StatusOK, map[string]any{"profile": a.Profile, "addons": addons})
			return
		}
	}
	s.fail(w, r, http.StatusNotFound, "app not installed")
}

type installRequest struct {
	// Coordinate is the store coordinate <catalogue>/<app> the profile was
	// offered under; the entitlement is recorded against it.
	Coordinate string `json:"coordinate"`
}

func (s *Server) install(w http.ResponseWriter, r *http.Request, c call) {
	ctx := r.Context()
	tenant, profile := r.PathValue("t"), r.PathValue("p")
	if s.cfg.EnforceEntitlements {
		var body installRequest
		if err := decode(r, &body); err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid body")
			return
		}
		entry, err := authz.CatalogueEntry(body.Coordinate)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "coordinate is required: <catalogue>/<app>")
			return
		}
		// The second question is about the tenant, not the person: is this
		// tenant entitled to this entry, now.
		ok, err := s.cfg.Authz.Check(ctx, reqID(ctx), authz.Tenant(tenant), "can_install", entry)
		if err != nil {
			s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		if !ok {
			s.fail(w, r, http.StatusForbidden, "tenant is not entitled to this catalogue entry")
			return
		}
	}
	res, err := s.cfg.Repo.Install(ctx, tenant, profile, c.meta)
	s.written(w, r, res, err)
}

func (s *Server) uninstall(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.Uninstall(r.Context(), r.PathValue("t"), r.PathValue("p"), c.meta)
	s.written(w, r, res, err)
}

type addonsRequest struct {
	Addons []string `json:"addons"`
}

func (s *Server) setAddons(w http.ResponseWriter, r *http.Request, c call) {
	var body addonsRequest
	if err := decode(r, &body); err != nil || body.Addons == nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"addons": [...]}`)
		return
	}
	res, err := s.cfg.Repo.SetAddons(r.Context(), r.PathValue("t"), r.PathValue("p"), body.Addons, c.meta)
	s.written(w, r, res, err)
}

// written answers a write. A change that landed is 202: git has it, the
// cluster does not yet, and the commit is the handle to ask about it. A
// request that changed nothing is 200 — the state asked for already holds.
func (s *Server) written(w http.ResponseWriter, r *http.Request, res gitops.Result, err error) {
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if !res.Changed {
		s.json(w, http.StatusOK, map[string]any{"status": res.Status})
		return
	}
	s.json(w, http.StatusAccepted, map[string]any{"status": res.Status, "commit": res.Commit})
}

func (s *Server) repoError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, gitops.ErrTenantNotFound):
		s.fail(w, r, http.StatusNotFound, "tenant not found")
	case errors.Is(err, gitops.ErrInvalidName):
		s.fail(w, r, http.StatusBadRequest, "invalid name")
	case errors.Is(err, gitops.ErrPushContended):
		w.Header().Set("Retry-After", "2")
		s.fail(w, r, http.StatusConflict, "repository is contended; retry")
	default:
		// Git's output can name paths and remotes; it goes to the log only.
		s.cfg.Log.ErrorContext(r.Context(), "repository operation failed", "request_id", reqID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusInternalServerError, "repository operation failed")
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.json(w, code, map[string]any{"error": msg, "request_id": reqID(r.Context())})
}

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
