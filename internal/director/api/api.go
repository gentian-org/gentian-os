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
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/entitlement"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
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
	ClusterSettingValues(ctx context.Context) (map[string]string, error)
	SetClusterSettings(ctx context.Context, values map[string]string, meta gitops.Meta) (gitops.Result, error)
	TenantDetails(ctx context.Context) ([]gitops.Tenant, error)
	CreateTenant(ctx context.Context, req gitops.NewTenant, meta gitops.Meta) (gitops.Result, error)
	RetireTenant(ctx context.Context, tenant string, meta gitops.Meta) (gitops.Result, error)
	Tenants(ctx context.Context) ([]string, error)
	SetResourcePlan(ctx context.Context, tenant string, plan gitops.Plan, meta gitops.Meta) (gitops.Result, error)
	SetTenantBackupPolicy(ctx context.Context, tenant string, policy gitops.BackupPolicy, meta gitops.Meta) (gitops.Result, error)
	ClearTenantBackupPolicy(ctx context.Context, tenant string, meta gitops.Meta) (gitops.Result, error)
	SetClusterBackupPolicy(ctx context.Context, policy gitops.BackupPolicy, meta gitops.Meta) (gitops.Result, error)
}

// Lifecycle is the operator's app-lifecycle API, read and never written: what
// the cluster enforces for a tenant, what is committed under it, and which
// plans the tenant may move to. See internal/director/lifecycle.
type Lifecycle interface {
	Get(ctx context.Context, path string, query url.Values) (int, []byte, error)
	Plans(ctx context.Context, tenant string, selfService bool) ([]lifecycle.Plan, error)
	// Do asks the cluster to do something once, as the person named. Only
	// the action routes call it.
	Do(ctx context.Context, path, actor string, body any) (int, []byte, error)
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
	// Cluster is the id of the one cluster this director serves: the object
	// cluster verbs are checked against, and the only {c} the routes accept.
	Cluster string
	// Store verifies and applies what the App Store signed. Nil leaves the
	// write unregistered: a cluster with no pinned store key believes no store.
	Store *StoreConfig
	// TilesPath is the file the operator's tile catalogue is projected into,
	// which is its ConfigMap mounted into this pod.
	//
	// Mounted rather than fetched: the director holds a git credential and no
	// cluster credential, and reading one ConfigMap is not worth giving it a
	// ServiceAccount token that can read the API at all. The kubelet keeps the
	// file in step with the ConfigMap, so the file is read per request and the
	// catalogue is never older than the projection by more than the kubelet's
	// own refresh.
	//
	// Empty, or a path that does not exist, is an empty catalogue.
	TilesPath string
	// Lifecycle answers what only the cluster knows about a tenant's
	// resources. Nil leaves the resources routes unregistered: a director
	// with no operator to ask has nothing to relay and nothing to validate a
	// plan against.
	Lifecycle Lifecycle
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

// identified registers a route that needs a caller and no relation: the
// caller asking about themselves. Everything else this server serves is
// guarded, and the difference is deliberate, so it is spelled out here rather
// than done by passing an empty relation to guarded.
func (s *Server) identified(pattern string, h func(http.ResponseWriter, *http.Request, call)) {
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
		h(w, r, call{id: ident, user: user, meta: gitops.Meta{
			Author: gitops.Person{Name: ident.Name, Email: ident.Email}, Subject: user[len("user:"):], RequestID: reqID(ctx),
		}})
	})
}

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
//
// This API serves three kinds of thing and says which in the route itself:
//
//	GET  <resource>            a READ of state. Answers what is.
//	PUT/PATCH/DELETE <resource>  a WRITE of declared state. Answers a commit:
//	                           202 with one, or 200 "unchanged". Git has it;
//	                           the cluster does not yet.
//	POST <resource>/actions/x  an ACTION. Answers what was started, not what
//	                           was committed, because nothing was.
//
// The difference is not decoration. A write says what should be true from now
// on and is reviewable in git for ever; an action happens once and leaves no
// commit, so the object it creates carries who asked instead. Registering an
// action through `action` rather than `guarded` is what keeps a handler from
// answering in the wrong shape.
func (s *Server) guarded(pattern, relation string, obj object, h func(http.ResponseWriter, *http.Request, call)) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if c, ok := s.authorize(w, r, pattern, relation, obj); ok {
			h(w, r, c)
		}
	})
}

// action registers something that happens once. The pattern must be a POST
// under `/actions/`, which is checked here rather than trusted: a route that
// looked like a read and made something happen would be the one mistake this
// distinction exists to prevent.
func (s *Server) action(pattern, relation string, obj object, h func(http.ResponseWriter, *http.Request, call)) {
	if !strings.HasPrefix(pattern, "POST ") || !strings.Contains(pattern, "/actions/") {
		panic("api: an action must be a POST under /actions/: " + pattern)
	}
	s.guarded(pattern, relation, obj, h)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// No membership endpoint. Keycloak states a user's groups to the
	// operator, which projects them into the graph along with the rest of its
	// structure. This service reads the graph to decide a call and writes git;
	// it writes no tuples, and its token cannot.

	// Reads are authorised by can_view, never by the write relation: whoever
	// may see a tenant may see what it has installed, whether or not they may
	// change it.
	s.guarded("GET /v1/tenants/{t}/apps", "can_view", tenantObject, s.listApps)
	s.guarded("GET /v1/tenants/{t}/apps/{p}/addons", "can_view", tenantObject, s.getAddons)

	s.guarded("GET /v1/tenants/{t}/entitlements", "can_view", tenantObject, s.listEntitlements)

	// What the caller holds on a tenant, for the desktop: it renders from
	// the answer -- the admin tile by can_administer, the store by
	// can_install_app -- and decides nothing itself (ui-restructure.md §2).
	// Entered under can_enter, the relation that reaches the desktop at all.
	s.guarded("GET /v1/tenants/{t}/me", "can_enter", tenantObject, s.tenantMe)

	// The tiles this cluster offers, for the console that shows them. What
	// exists is the operator's projection; what this caller may open is asked
	// here. Entered under can_audit — the widest cluster relation — then each
	// tile is filtered by the relation it names on the object it names.
	if s.cfg.Cluster != "" {
		s.guarded("GET /v1/clusters/{c}/tiles", "can_audit", s.clusterObject, s.clusterTiles)
		// The cluster's settings: what they are, and changing them.
		//
		// Reading is can_audit, the widest cluster relation, because a
		// setting is not a secret and someone who may look at the cluster may
		// see how it is configured. Writing is can_configure, which model v1
		// defines as exactly this: "Cluster claim, plans, ceilings, network".
		// Every change is a commit to the claim in git with the person as its
		// author, and the operator acts on it from there.
		s.guarded("GET /v1/clusters/{c}/settings", "can_audit", s.clusterObject, s.clusterSettings)
		s.guarded("PATCH /v1/clusters/{c}/settings", "can_configure", s.clusterObject, s.setClusterSettings)
		// Bringing a tenant on is the errand the console exists for, and it
		// is one commit. Listing is can_audit because seeing which customers
		// a cluster carries is a read; creating and retiring are
		// can_configure because they change what the cluster runs.
		s.guarded("GET /v1/clusters/{c}/tenants", "can_audit", s.clusterObject, s.listTenants)
		s.guarded("POST /v1/clusters/{c}/tenants", "can_configure", s.clusterObject, s.createTenant)
		s.guarded("DELETE /v1/clusters/{c}/tenants/{t}", "can_configure", s.clusterObject, s.retireTenant)
		// Who the caller is at cluster scope: which of the cluster's verbs
		// they hold. Identified, not guarded: a person with no cluster
		// relation at all is answered with every verb false, because "you
		// hold nothing here" is the ordinary answer for almost everyone who
		// signs in, and a console has to render that rather than an error.
		s.identified("GET /v1/clusters/{c}/me", s.clusterMe)
	}
	if s.cfg.Store != nil {
		s.mux.HandleFunc("POST /v1/tenants/{t}/entitlements", s.entitle)
	}

	s.guarded("POST /v1/tenants/{t}/apps/{p}", "can_install_app", tenantObject, s.install)
	s.guarded("DELETE /v1/tenants/{t}/apps/{p}", "can_install_app", tenantObject, s.uninstall)
	s.guarded("PUT /v1/tenants/{t}/apps/{p}/addons", "can_install_app", tenantObject, s.setAddons)

	// A tenant's resources: the ceiling the cluster enforces, what is under
	// it, the plans it may move to, and its history. The reads are the
	// operator's answers relayed under can_view. The one write, choosing a
	// plan, is can_set_plan, model v1's own verb for it, and it is a commit:
	// the operator learns the plan from git like everything else.
	if s.cfg.Lifecycle != nil {
		s.guarded("GET /v1/tenants/{t}/resources", "can_view", tenantObject, s.resourceState)
		s.guarded("GET /v1/tenants/{t}/resources/plans", "can_view", tenantObject, s.resourcePlans)
		s.guarded("GET /v1/tenants/{t}/resources/usage", "can_view", tenantObject, s.resourceUsage)
		s.guarded("GET /v1/tenants/{t}/resources/report", "can_view", tenantObject, s.resourceReport)
		s.guarded("PUT /v1/tenants/{t}/resources", "can_set_plan", tenantObject, s.setResourcePlan)
		if s.cfg.Cluster != "" {
			// Every tenant's ceiling in one answer, for the platform
			// operator's view. can_audit, like the tenant list it is made
			// from.
			s.guarded("GET /v1/clusters/{c}/resources", "can_audit", s.clusterObject, s.clusterResources)
		}

		// A tenant's backups: what exists, what each run did, the policy in
		// force once inheritance is resolved, and when the next scheduled
		// run is. All reads, all relayed from the operator under can_view --
		// whoever may see a tenant may see whether its data is being kept.
		// Changing a policy is a commit, and is not here yet.
		s.guarded("GET /v1/tenants/{t}/backups", "can_view", tenantObject, s.tenantBackups)
		s.guarded("GET /v1/tenants/{t}/backups/{name}", "can_view", tenantObject, s.tenantBackup)
		s.guarded("GET /v1/tenants/{t}/backup-policy", "can_view", tenantObject, s.tenantBackupPolicy)
		s.guarded("GET /v1/tenants/{t}/backup-schedules", "can_view", tenantObject, s.tenantBackupSchedules)
		if s.cfg.Cluster != "" {
			// The cluster's own policy, and every tenant's schedules. Read
			// under can_audit: what the platform keeps, and for how long, is
			// something whoever may look at the cluster may see.
			s.guarded("GET /v1/clusters/{c}/backup-policy", "can_audit", s.clusterObject, s.clusterBackupPolicy)
			s.guarded("GET /v1/clusters/{c}/backup-schedules", "can_audit", s.clusterObject, s.clusterBackupSchedules)
		}

		// Writing a backup policy is writing declared state: what should be
		// true of every run from now on. It is a commit, under
		// can_set_policy -- model v1's own verb for the policies of a tenant
		// -- and the cluster's is can_configure, like every other thing the
		// cluster declares about itself. Clearing a tenant's is the same
		// kind of write: it stops declaring, and inherits again.
		s.guarded("PUT /v1/tenants/{t}/backup-policy", "can_set_policy", tenantObject, s.setTenantBackupPolicy)
		s.guarded("DELETE /v1/tenants/{t}/backup-policy", "can_set_policy", tenantObject, s.clearTenantBackupPolicy)
		if s.cfg.Cluster != "" {
			s.guarded("PUT /v1/clusters/{c}/backup-policy", "can_configure", s.clusterObject, s.setClusterBackupPolicy)
		}

		// Taking a backup is not declaring anything: it happens once, now.
		// can_administer, because it reads every store the tenant has and
		// writes a bundle somebody can restore from.
		s.action("POST /v1/tenants/{t}/actions/backup", "can_administer", tenantObject, s.startBackup)
		s.action("POST /v1/tenants/{t}/actions/delete-backup", "can_administer", tenantObject, s.deleteBackup)
	}
}

// clusterRelations are the cluster verbs a console asks about the caller: the
// ones that decide which screens exist for them. They are the permissions of
// type cluster in the model, and the vocabulary check keeps this list honest.
var clusterRelations = []string{
	"can_configure", "can_deploy_tenant", "can_operate_system", "can_install_shared",
	"can_grant_shared", "can_approve", "can_set_admission", "can_edit_raw", "can_audit",
}

// clusterMe answers which cluster verbs the caller holds.
func (s *Server) clusterMe(w http.ResponseWriter, r *http.Request, c call) {
	if r.PathValue("c") != s.cfg.Cluster {
		s.fail(w, r, http.StatusNotFound, "unknown cluster")
		return
	}
	target := authz.Cluster(s.cfg.Cluster)
	relations := make(map[string]bool, len(clusterRelations))
	for _, rel := range clusterRelations {
		ok, err := s.cfg.Authz.Check(r.Context(), reqID(r.Context()), c.user, rel, target)
		if err != nil {
			s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		relations[rel] = ok
	}
	s.json(w, http.StatusOK, map[string]any{
		"cluster":   s.cfg.Cluster,
		"subject":   c.meta.Subject,
		"relations": relations,
	})
}

// tenantRelations are the tenant verbs the desktop asks about the caller.
var tenantRelations = []string{
	"can_enter", "can_view", "can_administer", "can_install_app", "can_manage_users",
	"can_set_plan", "can_set_policy", "can_grant", "can_approve_privilege", "can_expose",
}

func (s *Server) tenantMe(w http.ResponseWriter, r *http.Request, c call) {
	target, err := tenantObject(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return
	}
	relations := make(map[string]bool, len(tenantRelations))
	for _, rel := range tenantRelations {
		ok, err := s.cfg.Authz.Check(r.Context(), reqID(r.Context()), c.user, rel, target)
		if err != nil {
			s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		relations[rel] = ok
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant":    r.PathValue("t"),
		"subject":   c.meta.Subject,
		"relations": relations,
	})
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

// catalogue is what the operator projected, or nothing.
//
// A missing file is an empty catalogue and not a failure. That is what a
// cluster whose operator has not projected yet looks like, and it is also what
// a cluster running an operator too old to project looks like; in both cases
// the honest answer is that this director knows of no tiles, which a console
// can render. An error would put a red banner on the page for a condition that
// resolves itself on the operator's next pass.
func (s *Server) catalogue() ([]tilecatalogue.Tile, error) {
	if s.cfg.TilesPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(s.cfg.TilesPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := tilecatalogue.Parse(data)
	if err != nil {
		return nil, err
	}
	return c.Tiles, nil
}

// clusterTiles answers with the tiles this caller may open.
//
// The list of what exists is the operator's: it projects the consoles it
// routes and the exposures of installed components that declare a tile. This
// handler adds the one thing the projection cannot know, which is who is
// asking, and it asks the graph once per tile. A tile the caller may not open
// is not on the page, because leaving it there and refusing the click tells a
// person about a console they have no business knowing exists.
func (s *Server) clusterTiles(w http.ResponseWriter, r *http.Request, c call) {
	ctx := r.Context()
	domain, err := s.cfg.Repo.KernelDomain(ctx)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	catalogue, err := s.catalogue()
	if err != nil {
		s.cfg.Log.ErrorContext(ctx, "the tile catalogue cannot be read",
			"request_id", reqID(ctx), "path", s.cfg.TilesPath, "error", err.Error())
		s.fail(w, r, http.StatusServiceUnavailable, "the tile catalogue cannot be read")
		return
	}
	out := []tileOut{}
	for _, t := range catalogue {
		shown := false
		for _, rel := range t.AnyOf {
			ok, err := s.cfg.Authz.Check(ctx, reqID(ctx), c.user, rel, t.Object)
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
			out = append(out, tileOut{Name: t.Name, DisplayName: t.DisplayName, Description: t.Description, URL: t.URL, Icon: t.Icon})
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

// clusterSettings answers what this cluster is configured with, and what may
// be changed.
//
// The catalogue travels with the values so a console can render the screen
// from one answer: what each setting means, what it accepts, and what it is
// now. A setting the claim does not carry is absent rather than empty,
// because "unset, so the schema's default applies" and "set to nothing" are
// different answers.
func (s *Server) clusterSettings(w http.ResponseWriter, r *http.Request, _ call) {
	values, err := s.cfg.Repo.ClusterSettingValues(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	catalogue := gitops.ClusterSettings()
	out := make([]map[string]any, 0, len(catalogue))
	for _, c := range catalogue {
		entry := map[string]any{"path": c.Path, "doc": c.Doc}
		if len(c.OneOf) > 0 {
			entry["oneOf"] = c.OneOf
		}
		// What applies when the claim does not carry it. A screen that says
		// "unset, the default applies" is only useful if it can say which.
		if c.Default != "" {
			entry["default"] = c.Default
		}
		if v, ok := values[c.Path]; ok {
			entry["value"] = v
		}
		out = append(out, entry)
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": s.cfg.Cluster, "settings": out})
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request, _ call) {
	tenants, err := s.cfg.Repo.TenantDetails(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"cluster": s.cfg.Cluster, "tenants": tenants})
}

// createTenant writes one manifest and commits it.
//
// The name is the only thing that must be right, because it becomes a
// namespace, a realm, a database prefix and a hostname, and none of those can
// be renamed afterwards without moving data. So it is validated here and the
// refusal says which rule it broke rather than answering a bare 400.
func (s *Server) createTenant(w http.ResponseWriter, r *http.Request, c call) {
	var body gitops.NewTenant
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"name": "<name>", "displayName": "<name>"}`)
		return
	}
	if !gitops.ValidName(body.Name) {
		s.fail(w, r, http.StatusBadRequest,
			"a tenant name is a DNS label: lower-case letters, digits and hyphens, starting and ending with a letter or digit")
		return
	}
	res, err := s.cfg.Repo.CreateTenant(r.Context(), body, c.meta)
	if errors.Is(err, gitops.ErrTenantExists) {
		s.fail(w, r, http.StatusConflict, "a tenant of that name already exists")
		return
	}
	s.written(w, r, res, err)
}

// retireTenant stops git describing a tenant, which is what removes it.
//
// Whether its data goes with it is the manifest's deletionPolicy, honoured by
// the operator, not something decided here.
func (s *Server) retireTenant(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.RetireTenant(r.Context(), r.PathValue("t"), c.meta)
	if errors.Is(err, gitops.ErrTenantProtected) {
		s.fail(w, r, http.StatusForbidden, err.Error())
		return
	}
	s.written(w, r, res, err)
}

type clusterSettingsRequest struct {
	Settings map[string]string `json:"settings"`
}

// setClusterSettings writes the named settings to the claim in git.
//
// One commit for the whole request. A caller changing mail from the kernel's
// own stack to an external relay sets the mode and the host together, and a
// commit carrying only the first is a cluster that has stopped sending mail
// with no sign of why in the diff.
func (s *Server) setClusterSettings(w http.ResponseWriter, r *http.Request, c call) {
	var body clusterSettingsRequest
	if err := decode(r, &body); err != nil || len(body.Settings) == 0 {
		s.fail(w, r, http.StatusBadRequest, `body must be {"settings": {"<path>": "<value>"}}`)
		return
	}
	res, err := s.cfg.Repo.SetClusterSettings(r.Context(), body.Settings, c.meta)
	if errors.Is(err, gitops.ErrUnknownSetting) || errors.Is(err, gitops.ErrNotScalar) {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
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

// started answers an action. Not 202-with-a-commit, because there is no
// commit: the cluster was asked to do something and is doing it, and the
// handle is the object's name rather than a git id.
func (s *Server) started(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	if status < 200 || status > 299 {
		s.fail(w, r, status, lifecycle.ErrorMessage(body))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(body)
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
