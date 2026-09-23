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
	"github.com/gentian-org/gentian-os/internal/director/entitlement"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/tiles"
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
	Entitlements(ctx context.Context, tenant string) ([]gitops.Entitlement, error)
	KernelDomain(ctx context.Context) (string, error)
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
	// Logout receives the zone clients' back-channel logouts and records the
	// ended session for every shim replica (networking.md §4). Authenticated
	// by the issuer's signature on the logout token, not by a bearer. Nil
	// leaves the endpoint unregistered.
	Logout http.Handler
	// Cluster is the id of the one cluster this director serves: the object
	// cluster verbs are checked against, and the only {c} the routes accept.
	Cluster string
	// Store verifies and applies what the App Store signed. Nil leaves the
	// write unregistered: a cluster with no pinned store key believes no store.
	Store *StoreConfig
}

// StoreConfig is what the entitlement write needs.
type StoreConfig struct {
	Verifier *entitlement.Verifier
	Applier  *entitlement.Applier
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

// clusterObject accepts only this director's own cluster. Another id is not
// forbidden, it does not exist here.
func (s *Server) clusterObject(r *http.Request) (string, error) {
	if c := r.PathValue("c"); c != s.cfg.Cluster || c == "" {
		return "", gitops.ErrInvalidName
	}
	return authz.Cluster(s.cfg.Cluster), nil
}

func tenantObject(r *http.Request) (string, error) {
	t := r.PathValue("t")
	if !gitops.ValidName(t) {
		return "", gitops.ErrInvalidName
	}
	return authz.Tenant(t), nil
}

// authorize runs the steps every user-callable route shares: who is calling,
// may they do relation to the route's object. On refusal it has already
// answered, and ok is false.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, pattern, relation string, obj object) (c call, ok bool) {
	ctx := r.Context()
	ident, err := s.cfg.Authn.FromRequest(r)
	if err != nil {
		s.cfg.Log.WarnContext(ctx, "authentication failed", "request_id", reqID(ctx), "reason", err.Error(), "route", pattern)
		w.Header().Set("WWW-Authenticate", `Bearer realm="gentian-director"`)
		s.fail(w, r, http.StatusUnauthorized, "unauthenticated")
		return call{}, false
	}
	user, err := authz.User(ident.Subject)
	if err != nil {
		s.fail(w, r, http.StatusUnauthorized, "unauthenticated")
		return call{}, false
	}
	target, err := obj(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return call{}, false
	}
	allowed, err := s.cfg.Authz.Check(ctx, reqID(ctx), user, relation, target)
	if err != nil {
		// The decision point is unreachable: refuse, and say it is us.
		s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return call{}, false
	}
	if !allowed {
		s.fail(w, r, http.StatusForbidden, "forbidden")
		return call{}, false
	}
	return call{
		id:   ident,
		user: user,
		meta: gitops.Meta{
			Author:    gitops.Person{Name: ident.Name, Email: ident.Email},
			Subject:   user[len("user:"):],
			RequestID: reqID(ctx),
			Decision:  relation + " " + target,
		},
	}, true
}

// guarded registers a route with the relation it requires. There is no other
// way to register a route a user can call.
func (s *Server) guarded(pattern, relation string, obj object, h func(http.ResponseWriter, *http.Request, call)) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if c, ok := s.authorize(w, r, pattern, relation, obj); ok {
			h(w, r, c)
		}
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if s.cfg.Events != nil {
		s.mux.Handle("POST /v1/events/keycloak", s.cfg.Events)
	}
	if s.cfg.Logout != nil {
		s.mux.Handle("POST /v1/logout/keycloak", s.cfg.Logout)
	}

	// Reads are authorised by can_view, never by the write relation: whoever
	// may see a tenant may see what it has installed, whether or not they may
	// change it.
	s.guarded("GET /v1/tenants/{t}/apps", "can_view", tenantObject, s.listApps)
	s.guarded("GET /v1/tenants/{t}/apps/{p}/addons", "can_view", tenantObject, s.getAddons)

	s.guarded("GET /v1/tenants/{t}/entitlements", "can_view", tenantObject, s.listEntitlements)

	// The kernel's own UIs, for the cluster administrator's console. Entered
	// under can_audit — the widest cluster relation — then each tile is
	// filtered by its own.
	if s.cfg.Cluster != "" {
		s.guarded("GET /v1/clusters/{c}/tiles", "can_audit", s.clusterObject, s.kernelTiles)
	}
	if s.cfg.Store != nil {
		s.mux.HandleFunc("POST /v1/tenants/{t}/entitlements", s.entitle)
	}

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

func (s *Server) listEntitlements(w http.ResponseWriter, r *http.Request, _ call) {
	facts, err := s.cfg.Repo.Entitlements(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "entitlements": facts})
}

type tileOut struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Icon        string `json:"icon"`
}

func (s *Server) kernelTiles(w http.ResponseWriter, r *http.Request, c call) {
	ctx := r.Context()
	domain, err := s.cfg.Repo.KernelDomain(ctx)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	object := authz.Cluster(s.cfg.Cluster)
	out := []tileOut{}
	for _, t := range tiles.All() {
		shown := false
		for _, rel := range t.AnyOf {
			ok, err := s.cfg.Authz.Check(ctx, reqID(ctx), c.user, rel, object)
			if err != nil {
				s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
				return
			}
			if ok {
				shown = true
				break
			}
		}
		if shown {
			out = append(out, tileOut{Name: t.Name, DisplayName: t.DisplayName, Description: t.Description, URL: t.URL(domain), Icon: t.Icon})
		}
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": s.cfg.Cluster, "kernelDomain": domain, "tiles": out})
}

type entitleRequest struct {
	// Grant is the store's statement, a compact JWS.
	Grant string `json:"grant"`
}

// entitle takes a statement the store signed. The signature is what is
// believed, so it is checked first, before anything is asked of the caller.
//
// A grant adds access, so it also needs a person who may install in this
// tenant to be the one delivering it: the store may trigger, it may not decide
// for the tenant. A revocation only removes access and the store's signature is
// all the authority it needs — requiring the tenant's own administrator to
// deliver it would let them decline to.
func (s *Server) entitle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenant := r.PathValue("t")
	if !gitops.ValidName(tenant) {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return
	}
	var body entitleRequest
	if err := decode(r, &body); err != nil || body.Grant == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"grant": "<compact JWS>"}`)
		return
	}
	claims, kid, err := s.cfg.Store.Verifier.Verify(body.Grant, tenant)
	switch {
	case errors.Is(err, entitlement.ErrNotBelieved):
		s.cfg.Log.WarnContext(ctx, "entitlement statement refused", "request_id", reqID(ctx), "tenant", tenant, "reason", err.Error())
		s.fail(w, r, http.StatusUnauthorized, "statement is not verifiably the store's")
		return
	case errors.Is(err, entitlement.ErrNotForHere):
		s.fail(w, r, http.StatusForbidden, "statement is not for this cluster and tenant")
		return
	case err != nil:
		s.fail(w, r, http.StatusBadRequest, "statement is incomplete")
		return
	}

	meta := gitops.Meta{RequestID: reqID(ctx), Principal: "store:" + kid, Decision: "signed " + claims.ID}
	if claims.Granted {
		c, ok := s.authorize(w, r, "POST /v1/tenants/{t}/entitlements", "can_install_app", tenantObject)
		if !ok {
			return
		}
		// The person delivered it; the store decided it. Both are recorded.
		meta = c.meta
		meta.Decision += "; store:" + kid + " signed " + claims.ID
	}
	res, err := s.cfg.Store.Applier.Apply(ctx, tenant, claims, kid, meta)
	if errors.Is(err, gitops.ErrStaleFact) {
		s.fail(w, r, http.StatusConflict, "a newer statement about this entry is already recorded")
		return
	}
	s.written(w, r, res, err)
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
	case errors.Is(err, gitops.ErrNoClusterClaim):
		s.fail(w, r, http.StatusNotFound, "this cluster has no Cluster claim in the repository")
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
