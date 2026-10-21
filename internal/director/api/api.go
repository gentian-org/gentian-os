/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/platformversion"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

// Authenticator establishes the caller's identity from a request.
type Authenticator interface {
	FromRequest(r *http.Request) (*authn.Identity, error)
}

// Repository is the git backend.
type Repository interface {
	InstallFrom(ctx context.Context, tenant, profile, digest, catalogue string, defaultGrant *bool, meta gitops.Meta) (gitops.Result, error)
	RetryMove(ctx context.Context, tenant, profile, digest string, meta gitops.Meta) (gitops.Result, error)
	Uninstall(ctx context.Context, tenant, profile string, meta gitops.Meta) (gitops.Result, error)
	SetAddonsPinned(ctx context.Context, tenant, profile string, addons []string, pins []gitops.AddonPin, meta gitops.Meta) (gitops.Result, error)
	Apps(ctx context.Context, tenant string) ([]gitops.App, error)
	Tenants(ctx context.Context) ([]string, error)
	IsPlatformTenant(ctx context.Context, tenant string) (bool, error)
	KernelDomain(ctx context.Context) (string, error)
	ClusterSettingValues(ctx context.Context) (map[string]string, error)
	SetClusterSettings(ctx context.Context, values map[string]string, meta gitops.Meta) (gitops.Result, error)
	TenantDetails(ctx context.Context) ([]gitops.Tenant, error)
	CreateTenant(ctx context.Context, req gitops.NewTenant, meta gitops.Meta) (gitops.Result, error)
	AdminsApproveAtCreation(ctx context.Context, req gitops.NewTenant) (bool, error)
	RetireTenant(ctx context.Context, tenant string, meta gitops.Meta) (gitops.Result, error)
	RequestTenantPurge(ctx context.Context, tenant string, now time.Time, opts gitops.PurgeOptions, meta gitops.Meta) (gitops.Result, error)
	DeclareTenant(ctx context.Context, imported gitops.ImportedTenant, meta gitops.Meta) (gitops.Result, error)
	PendingImport(ctx context.Context, tenant string) (gitops.PendingImport, bool, error)
	PendingImports(ctx context.Context) ([]gitops.PendingImport, error)
	FinishImport(ctx context.Context, tenant string, meta gitops.Meta) (gitops.Result, error)
	PendingPurges(ctx context.Context) ([]string, error)
	SetTenantDomain(ctx context.Context, tenant, domain string, meta gitops.Meta) (gitops.Result, error)
	CheckImportedDomain(ctx context.Context, tenant, domain string) (string, error)
	TenantBrokeredFrom(ctx context.Context, tenant string) ([]gitops.BrokeredFrom, error)
	TenantGroups(ctx context.Context, tenant string) ([]string, error)
	SetTenantGroups(ctx context.Context, tenant string, groups []string, meta gitops.Meta) (gitops.Result, error)
	TenantSignIn(ctx context.Context, tenant string) ([]gentianov1alpha1.TenantSignInFrom, []string, error)
	SetTenantSignIn(ctx context.Context, target, source string, groups []string, remove bool, meta gitops.Meta) (gitops.Result, error)
	SetTenantBrokeredFrom(ctx context.Context, tenant string, entry gitops.BrokeredFrom, meta gitops.Meta) (gitops.Result, error)
	RemoveTenantBrokeredFrom(ctx context.Context, tenant, name string, meta gitops.Meta) (gitops.Result, error)
	SetExposureAlias(ctx context.Context, tenant, install, exposure, host string, meta gitops.Meta) (gitops.Result, error)
	TenantPlacement(ctx context.Context, tenant string) (gitops.TenantPlacement, error)
	ClusterBranding(ctx context.Context) (*gentianov1alpha1.BrandingSpec, bool, error)
	SetClusterBranding(ctx context.Context, spec gentianov1alpha1.BrandingSpec, meta gitops.Meta) (gitops.Result, error)
	ClusterModels(ctx context.Context) (gitops.ModelSettings, error)
	SetClusterModels(ctx context.Context, want gitops.ModelSettings, meta gitops.Meta) (gitops.Result, error)
	SetResourcePlan(ctx context.Context, tenant string, plan gitops.Plan, meta gitops.Meta) (gitops.Result, error)
	SetTenantBackupPolicy(ctx context.Context, tenant string, policy gitops.BackupPolicy, meta gitops.Meta) (gitops.Result, error)
	ClearTenantBackupPolicy(ctx context.Context, tenant string, meta gitops.Meta) (gitops.Result, error)
	SetClusterBackupPolicy(ctx context.Context, policy gitops.BackupPolicy, meta gitops.Meta) (gitops.Result, error)
	TenantSecurityPolicy(ctx context.Context, tenant string) (*gitops.SecurityPolicy, error)
	SetTenantSecurityPolicy(ctx context.Context, tenant string, policy gitops.SecurityPolicy, meta gitops.Meta) (gitops.Result, error)
	MaterialiseProfile(ctx context.Context, name, digest string, body []byte, origin string, meta gitops.Meta) (gitops.Result, error)
	ProfileOnCluster(ctx context.Context, name string) (gitops.MaterialisedProfile, error)
	BuildOnCluster(ctx context.Context, name, digest string) (gitops.StoredBuild, bool, error)
	UpdatePolicy(ctx context.Context) (gitops.UpdatePolicy, error)
	SetAppUpdateMode(ctx context.Context, tenant, profile, mode string, meta gitops.Meta) (gitops.Result, error)
	ProfileDefinition(ctx context.Context, name string) (*gentianov1alpha1.ComponentProfile, error)
	CatalogueDeclares(ctx context.Context, kind, name string) (string, error)
	RetireProfile(ctx context.Context, name string, meta gitops.Meta) (gitops.Result, error)
	Catalogue(ctx context.Context) (gitops.CatalogueSettings, error)
	TenantCatalogue(ctx context.Context, tenant string) (gitops.TenantCatalogue, error)
	TenantCatalogues(ctx context.Context) (map[string]gitops.TenantCatalogue, error)
	AddClusterCatalogueSource(ctx context.Context, name, address string, meta gitops.Meta) (gitops.Result, error)
	RemoveClusterCatalogueSource(ctx context.Context, name string, meta gitops.Meta) (gitops.Result, error)
	AddTenantCatalogueSource(ctx context.Context, tenant, name, address string, by gitops.TenantCatalogueActor, meta gitops.Meta) (gitops.Result, error)
	AddClusterCatalogueOCI(ctx context.Context, name string, oci gitops.CatalogueOCI, meta gitops.Meta) (gitops.Result, error)
	AddTenantCatalogueOCI(ctx context.Context, tenant, name string, oci gitops.CatalogueOCI, by gitops.TenantCatalogueActor, meta gitops.Meta) (gitops.Result, error)
	// RegistryRepository answers a repository owner declared ("" for the
	// cluster), as a catalogue kept as an OCI artifact is read with it.
	RegistryRepository(ctx context.Context, owner, name string) (catalogue.DeclaredRegistry, error)
	RemoveTenantCatalogueSource(ctx context.Context, tenant, name string, by gitops.TenantCatalogueActor, meta gitops.Meta) (gitops.Result, error)
	SetTenantCatalogueDelegation(ctx context.Context, tenant string, delegated bool, meta gitops.Meta) (gitops.Result, error)
	SetTenantAdminsApprove(ctx context.Context, tenant string, approve bool, meta gitops.Meta) (gitops.Result, error)
	TenantExposures(ctx context.Context, tenant string) ([]gitops.Exposure, error)
	PublishExposure(ctx context.Context, tenant string, e gitops.Exposure, meta gitops.Meta) (gitops.Result, error)
	WithdrawExposure(ctx context.Context, tenant, install, exposure string, meta gitops.Meta) (gitops.Result, error)
	TenantLocales(ctx context.Context, tenant string) ([]string, error)
	SetTenantLocales(ctx context.Context, tenant string, locales []string, meta gitops.Meta) (gitops.Result, error)
	TenantCountry(ctx context.Context, tenant string) (string, error)
	SetTenantCountry(ctx context.Context, tenant, country string, meta gitops.Meta) (gitops.Result, error)
	TenantPrivileges(ctx context.Context, tenant string) ([]gitops.PrivilegeGrant, error)
	GrantPrivilege(ctx context.Context, tenant string, grant gitops.PrivilegeGrant, meta gitops.Meta) (gitops.Result, error)
	RevokePrivilege(ctx context.Context, tenant, install, privilege string, meta gitops.Meta) (gitops.Result, error)
	TenantChanges(ctx context.Context, tenant string, limit int, since string) ([]gitops.Change, error)
	TenantExposureChanges(ctx context.Context, tenant string, limit int, since string) ([]gitops.Change, error)
	ClusterChanges(ctx context.Context, limit int, since string) ([]gitops.Change, error)
	SetAppGrant(ctx context.Context, tenant, app string, grant gitops.AppGrant, meta gitops.Meta) (gitops.Result, error)
	ClearAppGrant(ctx context.Context, tenant, app string, meta gitops.Meta) (gitops.Result, error)
	PlatformSecurity(ctx context.Context) ([]gitops.MacWaiver, error)
	SetPlatformSecurity(ctx context.Context, waivers []gitops.MacWaiver, meta gitops.Meta) (gitops.Result, error)
	PlatformClusterRoles(ctx context.Context) ([]gitops.ClusterRoleAllowance, error)
	SetPlatformClusterRoles(ctx context.Context, roles []gitops.ClusterRoleAllowance, meta gitops.Meta) (gitops.Result, error)
	DeclareRepository(ctx context.Context, tenant, name string, d gitops.RepositoryDeclaration, meta gitops.Meta) (gitops.RepositoryResult, error)
	RemoveRepository(ctx context.Context, tenant, name, confirm string, meta gitops.Meta) (gitops.Result, error)
	BundleSources(ctx context.Context) ([]gitops.DeclaredBundleSource, error)
	DeclareBundleSource(ctx context.Context, name string, d gitops.BundleSourceDeclaration, meta gitops.Meta) (gitops.Result, error)
	RemoveBundleSource(ctx context.Context, name string, meta gitops.Meta) (gitops.Result, error)
}

// Lifecycle is the operator's listener as the director uses it: the commands
// of operator-split-plan.md §4.2, and the questions a commit or a command depends on -- which plans
// a tenant may move to, whether a restore finished, what the cluster still
// holds of a tenant being purged. No person's read of live state passes
// through here; those are the usher's. See internal/director/lifecycle.
type Lifecycle interface {
	Get(ctx context.Context, path string, query url.Values) (int, []byte, error)
	// Stream is Get for a body that is passed through byte for byte and may
	// be large: a bundle download. The caller closes the response body.
	Stream(ctx context.Context, path string) (*http.Response, error)
	// Upload is Do for a body streamed through unread: a bundle upload.
	Upload(ctx context.Context, path, contentType string, body io.Reader) (int, []byte, error)
	Plans(ctx context.Context, tenant string, selfService bool) ([]lifecycle.Plan, error)
	// Do asks the cluster to do something once, as the person named. Only
	// the action routes call it.
	Do(ctx context.Context, path, actor string, body any) (int, []byte, error)
}

// Config assembles a Server.
type Config struct {
	// UpdaterOff keeps the platform's own mover (updater.go) from running:
	// a test, or a cluster whose administrator moves everything by hand.
	UpdaterOff bool
	Authn      Authenticator
	Authz      authz.Checker
	// Viewer reads who holds what, for the console's read-only authorization
	// view. Optional: without it the routes do not exist, which a console
	// shows as the screen being unavailable rather than as an error on a
	// screen that should have worked.
	Viewer authz.Viewer
	Repo   Repository
	// Catalogue fetches a profile when a tenant installs it (AD-3), and a
	// catalogue's index when one is listed. Which catalogues there are is
	// not its to know: they are read from git when they are needed, per
	// tenant (catalogues.go). Nil is a director that fetches nothing, which
	// refuses every install from a catalogue and every catalogue added.
	Catalogue *catalogue.Fetcher
	// StoreURL is spec.catalogue.storeUrl: where a person is sent for
	// everything this cluster does not list for itself. Empty is a cluster
	// that belongs to no store.
	StoreURL string
	Log      *slog.Logger
	// Cluster is the id of the one cluster this director serves: the object
	// cluster verbs are checked against, and the only {c} the routes accept.
	Cluster string
	// Lifecycle carries out commands and answers what a commit depends on.
	// Nil leaves those routes unregistered: a director with no operator to
	// ask has nothing to command and nothing to validate a plan against.
	Lifecycle Lifecycle
	// CommissionKeys are the components that may commission organizations,
	// as the operator lists them (brokered_from.go). Nil recognises none.
	CommissionKeys *CommissionKeys
}

// Server is the director's API.
type Server struct {
	cfg Config
	mux *http.ServeMux
	// purging holds the tenants whose purge is being finished, so a second
	// request or a restart's resume does not start a second watcher.
	purging sync.Map
	// imports holds each import's progress by tenant, for the status route.
	imports sync.Map
	// importing holds the tenants whose import is being watched, so that a
	// repeated request or a restart's resume starts no second watcher.
	importing sync.Map
	// retiring holds the unused profiles whose deletion from the cluster is
	// being waited for, so a repeated request starts no second watcher.
	retiring sync.Map
	// background is the context every watcher waits under, stop ends it and
	// watchers counts them: what Close needs to end them and know they ended.
	background context.Context
	stop       context.CancelFunc
	watchers   sync.WaitGroup
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
	s.background, s.stop = context.WithCancel(context.Background())
	s.routes()
	s.startUpdater()
	return s, nil
}

// watch runs fn beside the requests, as something Close ends and waits for.
// The context it is given ends when the server is closed.
func (s *Server) watch(fn func(ctx context.Context)) {
	s.watchers.Add(1)
	go func() {
		defer s.watchers.Done()
		fn(s.background)
	}()
}

// Close ends what the server does beside answering requests -- the watchers
// of a purge, an import and a profile's deletion -- and returns when they
// have ended. Call it once the last request has been answered.
//
// A watcher that is waiting stops waiting. One that is writing to the
// repository finishes that write first: git is not interrupted half-way
// through a commit. What a watcher had left to do is in git, and the next
// start picks it up from there (ResumePurges, ResumeImports).
func (s *Server) Close() {
	s.stop()
	s.watchers.Wait()
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
	// The same report for one tenant: what of its apps is behind, and why.
	s.guarded("GET /v1/tenants/{t}/updates", "can_view", tenantObject, s.updatesForTenant)
	s.guarded("GET /v1/tenants/{t}/apps/{p}/addons", "can_view", tenantObject, s.getAddons)

	// The catalogues a tenant sees (AD-14): the cluster's and its own.
	// can_view, like every other read of a tenant: whoever may see a tenant
	// may see what it could install.
	s.guarded("GET /v1/tenants/{t}/catalogues", "can_view", tenantObject, s.listCatalogues)
	s.guarded("GET /v1/tenants/{t}/catalogues/{s}/entries", "can_view", tenantObject, s.listCatalogueEntries)
	// A tenant's administrator adding a catalogue for their own tenant.
	// can_install_app, the relation an install asks: a catalogue decides
	// what the tenant can install, and nobody who may not install should be
	// widening that. The relation is not the whole check -- the handler
	// refuses unless the cluster's administrator delegated this to the
	// tenant, and removes only what the tenant itself added.
	s.guarded("PUT /v1/tenants/{t}/catalogues/{s}", "can_install_app", tenantObject, s.addTenantCatalogue)
	s.guarded("DELETE /v1/tenants/{t}/catalogues/{s}", "can_install_app", tenantObject, s.removeTenantCatalogue)

	// What the caller holds on a tenant, for the desktop: it renders from
	// the answer -- the admin tile by can_administer, the store by
	// can_install_app -- and decides nothing itself (ui-restructure.md §2).
	// Entered under can_enter, the relation that reaches the desktop at all.
	s.guarded("GET /v1/tenants/{t}/me", "can_enter", tenantObject, s.tenantMe)

	// No tiles here. What a person may open is the usher's to answer
	// (internal/usher), for everybody who has a desktop and from a process
	// that holds no git credential.
	if s.cfg.Cluster != "" {
		// The cluster's settings: what they are, and changing them.
		//
		// Reading is can_audit, the widest cluster relation, because a
		// setting is not a secret and someone who may look at the cluster may
		// see how it is configured. Writing is can_configure, which model v1
		// defines as exactly this: "Cluster claim, plans, ceilings, network".
		// Every change is a commit to the claim in git with the person as its
		// author, and the operator acts on it from there.
		s.guarded("GET /v1/clusters/{c}/settings", "can_audit", s.clusterObject, s.clusterSettings)

		// Which installed apps are behind the build their catalogue offers,
		// and what keeps them there (updates.go); the move of all or some of
		// them to the newest build; and a tenant's override of how an app
		// moves. The report is the auditor's; the move and the override are
		// the cluster administrator's, under /v1/clusters so that a tenant's
		// administrator cannot give themselves the tenant mode.
		s.guarded("GET /v1/clusters/{c}/updates", "can_audit", s.clusterObject, s.updatesForCluster)
		s.action("POST /v1/clusters/{c}/actions/move-outdated", "can_configure", s.clusterObject, s.moveOutdated)
		s.guarded("PUT /v1/clusters/{c}/tenants/{t}/apps/{p}/update-mode", "can_configure", s.clusterObject, s.setAppUpdateMode)
		s.guarded("PATCH /v1/clusters/{c}/settings", "can_configure", s.clusterObject, s.setClusterSettings)
		// The brand every page shows: read like a setting, written like one.
		// The pages themselves read what the operator publishes from it,
		// never this route.
		s.guarded("GET /v1/clusters/{c}/branding", "can_audit", s.clusterObject, s.clusterBranding)
		s.guarded("PUT /v1/clusters/{c}/branding", "can_configure", s.clusterObject, s.setClusterBranding)
		// The models the gateway offers, which are the claim's and nobody
		// else's: read like a setting, written like one, and under
		// /v1/clusters only, so that no relation on a tenant reaches them --
		// a model spends the cluster's GPUs or a provider's account, for
		// every tenant. No provider's token is read or written here.
		s.guarded("GET /v1/clusters/{c}/models", "can_audit", s.clusterObject, s.clusterModels)
		s.guarded("PUT /v1/clusters/{c}/models", "can_configure", s.clusterObject, s.setClusterModels)
		// Bringing a tenant on is the errand the console exists for, and it
		// is one commit. Listing is can_audit because seeing which customers
		// a cluster carries is a read; creating and retiring are
		// can_configure because they change what the cluster runs.
		s.guarded("GET /v1/clusters/{c}/tenants", "can_audit", s.clusterObject, s.listTenants)
		s.guarded("POST /v1/clusters/{c}/tenants", "can_configure", s.clusterObject, s.createTenant)
		s.guarded("DELETE /v1/clusters/{c}/tenants/{t}", "can_configure", s.clusterObject, s.retireTenant)
		s.guarded("POST /v1/clusters/{c}/tenants/{t}/actions/purge", "can_configure", s.clusterObject, s.purgeTenant)
		// Where a tenant is served: a custom domain instead of
		// <tenant>.<kernel>, or none. can_configure, like creating the
		// tenant: it moves the tenant's hosts, mail and logins.
		s.guarded("PUT /v1/clusters/{c}/tenants/{t}/domain", "can_configure", s.clusterObject, s.setTenantDomain)
		s.guarded("DELETE /v1/clusters/{c}/tenants/{t}/domain", "can_configure", s.clusterObject, s.clearTenantDomain)
		// A tenant's own groups, and who signs in to it from another tenant
		// (tenant_sign_in.go).
		s.guarded("GET /v1/clusters/{c}/tenants/{t}/groups", "can_audit", s.clusterObject, s.tenantGroups)
		s.guarded("PUT /v1/clusters/{c}/tenants/{t}/groups", "can_configure", s.clusterObject, s.setTenantGroups)
		s.guarded("GET /v1/clusters/{c}/tenants/{t}/sign-in", "can_audit", s.clusterObject, s.tenantSignIn)
		s.guarded("PUT /v1/clusters/{c}/tenants/{t}/sign-in/{source}", "can_configure", s.clusterObject, s.setTenantSignIn)
		s.guarded("DELETE /v1/clusters/{c}/tenants/{t}/sign-in/{source}", "can_configure", s.clusterObject, s.removeTenantSignIn)
		// A second address for one published surface, directly under the
		// cluster's own domain. can_configure, and not the tenant's
		// approver: a name under the cluster's domain is the cluster's to
		// give. The approver publishes the surface first; this adds nothing
		// to what is public, only where it answers.
		s.guarded("PUT /v1/clusters/{c}/tenants/{t}/exposures/{inst}/{name}/alias", "can_configure", s.clusterObject, s.setExposureAlias)
		s.guarded("DELETE /v1/clusters/{c}/tenants/{t}/exposures/{inst}/{name}/alias", "can_configure", s.clusterObject, s.clearExposureAlias)
		// Where software may enter the cluster: its catalogues. Reading is
		// can_audit, like the settings; every change is can_configure -- a
		// catalogue for every tenant, a catalogue for one tenant, and
		// whether a tenant's own administrators may add theirs. The last is
		// under /v1/clusters and nowhere else, so that no relation on a
		// tenant reaches it.
		s.guarded("GET /v1/clusters/{c}/catalogues", "can_audit", s.clusterObject, s.listClusterCatalogues)
		s.guarded("PUT /v1/clusters/{c}/catalogues/{s}", "can_configure", s.clusterObject, s.addClusterCatalogue)
		s.guarded("DELETE /v1/clusters/{c}/catalogues/{s}", "can_configure", s.clusterObject, s.removeClusterCatalogue)
		s.guarded("PUT /v1/clusters/{c}/tenants/{t}/catalogues/{s}", "can_configure", s.clusterObject, s.addTenantCatalogueAsCluster)
		s.guarded("DELETE /v1/clusters/{c}/tenants/{t}/catalogues/{s}", "can_configure", s.clusterObject, s.removeTenantCatalogueAsCluster)
		s.guarded("PUT /v1/clusters/{c}/tenants/{t}/catalogue-delegation", "can_configure", s.clusterObject, s.delegateCatalogues)
		s.guarded("DELETE /v1/clusters/{c}/tenants/{t}/catalogue-delegation", "can_configure", s.clusterObject, s.undelegateCatalogues)
		// Whether a tenant's own administrators may approve what it
		// publishes to the internet. The same shape as the switch above and
		// for the same reason under /v1/clusters and nowhere else: a
		// tenant's administrator holds no relation that reaches it, so
		// nobody gives themselves the right to publish.
		s.guarded("PUT /v1/clusters/{c}/tenants/{t}/perimeter-delegation", "can_configure", s.clusterObject, s.delegatePerimeter)
		s.guarded("DELETE /v1/clusters/{c}/tenants/{t}/perimeter-delegation", "can_configure", s.clusterObject, s.undelegatePerimeter)
		if s.cfg.Lifecycle != nil {
			// Import: a bundle in, a tenant out (sovereignty-concept.md §4.3).
			s.guarded("POST /v1/clusters/{c}/bundles", "can_configure", s.clusterObject, s.uploadBundle)
			s.guarded("POST /v1/clusters/{c}/bundles/inspect", "can_configure", s.clusterObject, s.inspectBundle)
			// Removing one thing the catalogue left behind: an object no
			// bundle owns any more, or a profile nobody uses. The list is
			// the usher's read; this deletes, one named object at a time,
			// so it is can_configure and asks for the name again. An action
			// even where it also commits: what the caller is answered with
			// is what was deleted.
			s.action("POST /v1/clusters/{c}/actions/remove-catalogue-residue", "can_configure", s.clusterObject, s.removeCatalogueResidue)
			s.guarded("POST /v1/clusters/{c}/tenants/import", "can_configure", s.clusterObject, s.importTenant)
			s.guarded("GET /v1/clusters/{c}/tenants/{t}/import", "can_configure", s.clusterObject, s.importStatus)
			// A restore into an existing tenant, for the cluster's
			// administrator: the right an import needs, because a restore
			// replaces a tenant's data as an import makes it. The tenant's
			// own administrators restore under /v1/tenants (restore.go).
			s.guarded("POST /v1/clusters/{c}/tenants/{t}/bundles/inspect", "can_configure", s.clusterObject, s.inspectTenantBundle)
			s.action("POST /v1/clusters/{c}/tenants/{t}/actions/restore", "can_configure", s.clusterObject, s.restoreTenant)
			// Where a bundle is read from besides an upload (bundle_sources.go):
			// the cluster's declared sources, storage outside it whose address
			// is a commit and whose keys are the custodian's; and, for the
			// cluster's administrator, each tenant's own. can_configure, the
			// right an import needs. A fetch copies one bundle into the
			// import storage, from where it is downloaded, inspected,
			// imported or restored as an upload is.
			s.guarded("GET /v1/clusters/{c}/bundle-sources", "can_configure", s.clusterObject, s.listClusterSources)
			s.guarded("PUT /v1/clusters/{c}/bundle-sources/{name}", "can_configure", s.clusterObject, s.declareClusterSource)
			s.guarded("DELETE /v1/clusters/{c}/bundle-sources/{name}", "can_configure", s.clusterObject, s.removeClusterSource)
			s.guarded("GET /v1/clusters/{c}/bundle-sources/{name}/bundles", "can_configure", s.clusterObject, s.clusterSourceBundles)
			s.action("POST /v1/clusters/{c}/bundle-sources/{name}/actions/fetch", "can_configure", s.clusterObject, s.fetchFromClusterSource)
			s.guarded("GET /v1/clusters/{c}/tenants/{t}/bundle-sources", "can_configure", s.clusterObject, s.tenantSources)
			s.guarded("GET /v1/clusters/{c}/tenants/{t}/bundle-sources/{name}/bundles", "can_configure", s.clusterObject, s.tenantSourceBundles)
			s.action("POST /v1/clusters/{c}/tenants/{t}/bundle-sources/{name}/actions/fetch", "can_configure", s.clusterObject, s.fetchFromTenantSource)
			s.guarded("GET /v1/clusters/{c}/bundle-fetches/{f}", "can_configure", s.clusterObject, s.bundleFetch)
			s.guarded("GET /v1/clusters/{c}/bundles/{prefix}/download", "can_configure", s.clusterObject, s.importedBundleDownload)
		}
		// Who the caller is at cluster scope: which of the cluster's verbs
		// they hold. Identified, not guarded: a person with no cluster
		// relation at all is answered with every verb false, because "you
		// hold nothing here" is the ordinary answer for almost everyone who
		// signs in, and a console has to render that rather than an error.
		s.identified("GET /v1/clusters/{c}/me", s.clusterMe)
	}
	// The authorization state, read-only. can_audit at cluster scope and
	// can_view at tenant scope: the same relation that governs reading the
	// object each one describes.
	if s.cfg.Viewer != nil {
		if s.cfg.Cluster != "" {
			s.guarded("GET /v1/clusters/{c}/authorization", "can_audit", s.clusterObject, s.clusterAuthorization)
		}
		s.guarded("GET /v1/tenants/{t}/authorization", "can_view", tenantObject, s.tenantAuthorization)
	}

	// A tenant's outside realms: read by whoever may see the tenant, written
	// by the cluster's administrators and by a component the tenant names
	// (brokered_from.go).
	s.commissioned("GET /v1/tenants/{t}/brokered-from", "can_view", tenantObject, s.tenantBrokeredFrom)
	s.commissioned("PUT /v1/tenants/{t}/brokered-from/{name}", "can_configure", s.thisCluster, s.setTenantBrokeredFrom)
	s.commissioned("DELETE /v1/tenants/{t}/brokered-from/{name}", "can_configure", s.thisCluster, s.removeTenantBrokeredFrom)

	s.guarded("POST /v1/tenants/{t}/apps/{p}", "can_install_app", tenantObject, s.install)
	s.guarded("DELETE /v1/tenants/{t}/apps/{p}", "can_install_app", tenantObject, s.uninstall)
	s.guarded("PUT /v1/tenants/{t}/apps/{p}/addons", "can_install_app", tenantObject, s.setAddons)

	// WHO may answer follows from the KIND, so the two writes here are the
	// one place in this API where the relation on the route is not the
	// whole check. An egress request leaves the tenant's own namespace and
	// tenant#can_approve_privilege answers for it. A pod-security waiver
	// or a cluster role weakens what protects the NODE, and that is
	// cluster#can_approve -- the security officer's, who is deliberately
	// NOT a tenant administrator and must not have to become one to do
	// their job. Requiring both relations would mean exactly that, and
	// requiring only the tenant's would let a tenant administrator waive
	// a rule protecting every tenant on the node.
	//
	// So the route carries can_view, which is the floor -- you cannot
	// approve for a tenant you may not see -- and mayApprove makes the
	// decision per kind before either handler writes anything. It is
	// derived from the kind and never read from the request, or a profile
	// could ask for the cheaper approver.
	// What this tenant publishes to the internet, and who said it could.
	//
	// can_expose, never admin (AD-6). The members of the tenant's perimeter
	// group hold it, and the cluster's administrators while the tenant is
	// operated by its cluster; the tenant's own administrators only where
	// the cluster's administrator said so (perimeter-delegation above). It
	// is asked as its own relation, so putting something on the internet is
	// always its own line in the record rather than something that happened
	// while somebody was doing everything else.
	//
	// The READ is can_view, and it is the registry: every URL this tenant
	// publishes, who published it and until when. "What of ours is on the
	// internet" answered by the thing that put it there.
	s.guarded("GET /v1/tenants/{t}/exposures", "can_view", tenantObject, s.tenantExposures)
	s.guarded("PUT /v1/tenants/{t}/exposures/{inst}/{name}", "can_expose", tenantObject, s.publishExposure)
	s.guarded("DELETE /v1/tenants/{t}/exposures/{inst}/{name}", "can_expose", tenantObject, s.withdrawExposure)

	// The perimeter as those who answer for it see it: the same registry, and
	// its history. can_expose, so an approver who is not otherwise a member
	// of the tenant sees what they approve -- and nothing else of the tenant's.
	s.guarded("GET /v1/tenants/{t}/perimeter/exposures", "can_expose", tenantObject, s.tenantExposures)
	s.guarded("GET /v1/tenants/{t}/perimeter/history", "can_expose", tenantObject, s.perimeterHistory)

	// Where software comes from: the repositories a tenant, or the cluster,
	// installs from. The address is configuration -- it decides what may
	// enter -- so declaring one and removing one are commits with an author.
	// The password is not: it is set at the custodian, for a repository that
	// is already declared, and never passes through here.
	//
	// can_write_credential, on the cluster for the cluster's and on the
	// tenant for a tenant's: the relation the custodian asked when these
	// were its routes, so the same people may do this and nobody else. It is
	// also the relation that then lets them set the password, which is the
	// other half of the same errand.
	s.guarded("PUT /v1/tenants/{t}/repositories/{name}", "can_write_credential", tenantObject, s.declareTenantRepository)
	s.guarded("DELETE /v1/tenants/{t}/repositories/{name}", "can_write_credential", tenantObject, s.removeTenantRepository)
	if s.cfg.Cluster != "" {
		s.guarded("PUT /v1/clusters/{c}/repositories/{name}", "can_write_credential", s.clusterObject, s.declareClusterRepository)
		s.guarded("DELETE /v1/clusters/{c}/repositories/{name}", "can_write_credential", s.clusterObject, s.removeClusterRepository)
	}

	s.guarded("GET /v1/tenants/{t}/privileges", "can_view", tenantObject, s.tenantPrivileges)
	s.guarded("PUT /v1/tenants/{t}/privileges/{inst}/{kind}/{name}", "can_view", tenantObject, s.grantPrivilege)
	s.guarded("DELETE /v1/tenants/{t}/privileges/{inst}/{kind}/{name}", "can_view", tenantObject, s.revokePrivilege)

	// No reads of live state here. What the cluster made of a tenant -- the
	// state of its apps, its ceiling and usage, the plans it may move to, its
	// backups and their schedules, the policy in force, its integrations and
	// notices, and the cluster's own view of the same -- is the usher's to
	// answer (internal/usher), under the relation this server asked. This
	// process commits and commands; it does not answer for the cluster. What
	// it reads here is what git declares.
	//
	// The routes below need the operator all the same: a plan is validated
	// against the operator's answer before it is committed, and the commands
	// are carried out by it.
	if s.cfg.Lifecycle != nil {
		// The two acts on an app that are not desired state. Purging is
		// whoever may install's to do, as uninstalling is; provisioning hands
		// an app to people, which is can_grant's.
		s.guarded("POST /v1/tenants/{t}/actions/purge-app", "can_install_app", tenantObject, s.purgeApp)
		s.guarded("POST /v1/tenants/{t}/actions/provision-app", "can_grant", tenantObject, s.provisionApp)
		// Removing one piece a newer build of an app left on the cluster,
		// for the administrator of a tenant that has the app. Whoever may
		// install's, as uninstalling and purging are -- and not that alone:
		// the pieces are the cluster's, so the handler refuses unless this
		// is the only user tenant the cluster carries. The cluster's own
		// administrator removes them at
		// /v1/clusters/{c}/actions/remove-catalogue-residue.
		s.action("POST /v1/tenants/{t}/apps/{p}/actions/remove-residue", "can_install_app", tenantObject, s.removeAppResidue)
		// Asking the security officer for what the tenant cannot approve
		// itself (apps.go, requestPrivilege). Whoever may install asks; the
		// answer is the approval route above, decided by the kind.
		s.action("POST /v1/tenants/{t}/apps/{p}/actions/request-privilege", "can_install_app", tenantObject, s.requestPrivilege)

		// Choosing a plan is can_set_plan, model v1's own verb for it, and it
		// is a commit: the operator learns the plan from git like everything
		// else.
		s.guarded("PUT /v1/tenants/{t}/resources", "can_set_plan", tenantObject, s.setResourcePlan)

		// The bundle as one file, through the director and never a signed
		// URL: a link that works without a session is a session nobody can
		// revoke (sovereignty-concept.md §4.2). The one read of live state
		// left here: the usher's identity at the operator lists backups and
		// does not fetch one.
		s.guarded("GET /v1/tenants/{t}/backups/{name}/download", "can_view", tenantObject, s.tenantBundleDownload)

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

		// What an app may consume is declared state, and can_grant is model
		// v1's own verb for deciding it.
		s.guarded("PUT /v1/tenants/{t}/grants/{app}", "can_grant", tenantObject, s.setAppGrant)
		s.guarded("DELETE /v1/tenants/{t}/grants/{app}", "can_grant", tenantObject, s.clearAppGrant)
		if s.cfg.Cluster != "" {
			// What git declares may escape the default posture: the list
			// the PUT below replaces, read from where it is written so that
			// an edit starts from the last commit and not from the last
			// sync. What the cluster enforces at this moment, and which
			// profiles ask for a waiver, is the usher's answer on the same
			// path.
			s.guarded("GET /v1/clusters/{c}/platform-security", "can_audit", s.clusterObject, s.platformSecurity)
			// Changing what may escape the default posture is the cluster's
			// own security configuration: can_set_admission, which model v1
			// defines as exactly this.
			s.guarded("PUT /v1/clusters/{c}/platform-security", "can_set_admission", s.clusterObject, s.setPlatformSecurity)
		}

		// Publishing a notice to the people of a tenant is an action under
		// can_administer: it is not a statement about how the cluster should
		// be, it happens once, and nothing reconciles it.
		s.action("POST /v1/tenants/{t}/actions/notify", "can_administer", tenantObject, s.publishNotification)

		// What changed, who changed it, and what allowed them to. The
		// audit evidence the platform already had: every change to declared
		// state is a commit the director authored as the person and
		// trailered with the decision that permitted it.
		//
		// can_administer, not can_view: a change history names people and
		// what they were allowed to do, and an audit trail is something a
		// tenant's administrators read rather than its members.
		s.guarded("GET /v1/tenants/{t}/changes", "can_administer", tenantObject, s.tenantChanges)
		if s.cfg.Cluster != "" {
			s.guarded("GET /v1/clusters/{c}/changes", "can_audit", s.clusterObject, s.clusterChanges)
		}

		// The realm policy this tenant runs under: how strong a password
		// has to be, how long a session lasts, what happens after repeated
		// failures. Read from git, because git is where it is declared and
		// the composition is what applies it -- there is no Keycloak
		// credential anywhere in this path, which is the point. Written
		// under can_set_policy, like the backup policy beside it.
		s.guarded("GET /v1/tenants/{t}/security-policy", "can_view", tenantObject, s.tenantSecurityPolicy)
		s.guarded("PUT /v1/tenants/{t}/security-policy", "can_set_policy", tenantObject, s.setTenantSecurityPolicy)

		// The languages this tenant's realm offers on its login and account
		// pages (AD-15). Declared state like the policy above, for the same
		// reason: the composition that owns the realm is what writes it, so
		// nothing in this path holds a Keycloak credential and a realm rebuilt
		// from scratch comes back offering the same languages.
		//
		// can_set_policy, beside the security policy it sits with. Which
		// languages a tenant's people are offered is the tenant's to choose.
		s.guarded("GET /v1/tenants/{t}/locales", "can_view", tenantObject, s.tenantLocales)
		s.guarded("PUT /v1/tenants/{t}/locales", "can_set_policy", tenantObject, s.setTenantLocales)

		// The country the tenant is in, beside its languages and changed by
		// the same people. Declared in the manifest; a component whose
		// profile names countryKey is told it.
		s.guarded("GET /v1/tenants/{t}/country", "can_view", tenantObject, s.tenantCountry)
		s.guarded("PUT /v1/tenants/{t}/country", "can_set_policy", tenantObject, s.setTenantCountry)

		// The privileges this tenant's components asked for and what was
		// answered. A profile declaring a privilege is a request (AD-5); the
		// component waits until somebody answers, so these three routes are
		// what unblocks an install rather than a setting somebody tunes.
		//
		// Taking a backup is not declaring anything: it happens once, now.
		// can_administer, because it reads every store the tenant has and
		// writes a bundle somebody can restore from.
		s.action("POST /v1/tenants/{t}/actions/backup", "can_administer", tenantObject, s.startBackup)
		s.action("POST /v1/tenants/{t}/actions/delete-backup", "can_administer", tenantObject, s.deleteBackup)

		// Putting one of the tenant's bundles back into it: one of its own
		// backups, or a bundle of it uploaded here (restore.go). The data a
		// restore replaces is what a backup policy exists to keep, so it is
		// can_set_policy, the relation the tenant's backup policy is written
		// under: the tenant's administrators, and the cluster's while the
		// tenant consents to being operated. The cluster's administrator
		// restores under /v1/clusters as well, under the right an import
		// needs. Uploading is the first half of restoring a bundle that is
		// not a backup here, under the same relation.
		s.guarded("POST /v1/tenants/{t}/bundles", "can_set_policy", tenantObject, s.uploadBundle)
		s.guarded("POST /v1/tenants/{t}/bundles/inspect", "can_set_policy", tenantObject, s.inspectTenantBundle)
		s.action("POST /v1/tenants/{t}/actions/restore", "can_set_policy", tenantObject, s.restoreTenant)
		// Where the tenant's bundles are: its bucket in the cluster's storage
		// and its backup policy's destination, listed for its recovery
		// (bundle_sources.go). can_set_policy, the relation a restore and the
		// backup policy are asked under. Only bundles of this tenant are
		// listed; a destination's bucket may hold other tenants' too.
		s.guarded("GET /v1/tenants/{t}/bundle-sources", "can_set_policy", tenantObject, s.tenantSources)
		s.guarded("GET /v1/tenants/{t}/bundle-sources/{name}/bundles", "can_set_policy", tenantObject, s.tenantSourceBundles)
		// Fetching one of them into the cluster's import storage, to rewrite
		// it before it is restored: only a bundle whose header names the
		// tenant, counted towards the tenant's limits there, and its fetch
		// and its copy are the tenant's to read alone.
		s.action("POST /v1/tenants/{t}/bundle-sources/{name}/actions/fetch", "can_set_policy", tenantObject, s.fetchFromOwnSource)
		s.guarded("GET /v1/tenants/{t}/bundle-fetches/{f}", "can_set_policy", tenantObject, s.bundleFetch)
		s.guarded("GET /v1/tenants/{t}/bundles/{prefix}/download", "can_set_policy", tenantObject, s.importedBundleDownload)
	}

	// No people, groups or realm settings here. They are the registrar's
	// (internal/registrar), which holds the Keycloak credential they need;
	// this process holds none, so there is nothing here that could serve
	// them.
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
		// The platform's version line, which is no secret and which a
		// tenant's administrator restoring a bundle of an older one needs to
		// know: the cluster's settings carry it too, under can_audit.
		"platformVersionLine": backup.PlatformVersionLine,
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
			// The build each pinned addon is at, by name. Beside the list
			// rather than in it, as the manifest has it: the list is names
			// for every caller that reads it.
			pins := a.AddonPins
			if pins == nil {
				pins = []gitops.AddonPin{}
			}
			s.json(w, http.StatusOK, map[string]any{"profile": a.Profile, "addons": addons, "addonPins": pins})
			return
		}
	}
	s.fail(w, r, http.StatusNotFound, "app not installed")
}

type installRequest struct {
	// Coordinate is <catalogue>/<app>: which catalogue the profile comes
	// from. It is what materialising fetches, and its catalogue must be a
	// source the cluster declares; without it the install is of a profile
	// the cluster already has, and carries no digest.
	Coordinate string `json:"coordinate,omitempty"`
	// Digest is the content digest of the profile bundle, "sha256:<hex>":
	// the exact build being installed.
	//
	// It is the caller's to state -- the App Store's confirmation carries it,
	// and the cluster's own listing of a source gives it -- and it is not
	// signed. What it does is pin: only one set of bytes hashes to it, so the
	// source that serves the bundle can fail an install and cannot change
	// what is installed (AD-3). It is recorded with the install in git.
	Digest string `json:"digest,omitempty"`
	// DefaultGrant installs the app for everyone: every member of the tenant
	// has access to it by default. It is written on the app's entry in the
	// same commit, and the operator grants it once the app's group exists.
	// Absent leaves an installed app's entry as it is; false states that
	// access is given per person.
	DefaultGrant *bool `json:"defaultGrant,omitempty"`
}

// install commits an app to a tenant's manifest.
//
// The question asked is the route's own: may this person install apps in
// this tenant (can_install_app). Nothing here decides whether the tenant is
// licensed for the app. That is not the platform's to gate: whether the app
// arrives is decided where its artefacts are pulled, by whether the tenant
// holds a credential for the repository they come from.
//
// An install for everyone asks a second one. It gives people access to the
// app, which is what can_grant on the tenant guards wherever else that is
// done, so the request must pass both -- and is refused before anything is
// fetched or written when it does not.
func (s *Server) install(w http.ResponseWriter, r *http.Request, c call) {
	ctx := r.Context()
	tenant, profile := r.PathValue("t"), r.PathValue("p")
	if s.refusedInPlatformTenant(w, r, tenant) {
		return
	}
	// Read once: decoding twice would read an empty body the second time.
	var body installRequest
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid body")
			return
		}
	}
	if body.Digest != "" {
		digest, err := catalogue.CanonicalDigest(body.Digest)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "digest must be sha256:<64 hex characters>")
			return
		}
		body.Digest = digest
	}
	// Where the build comes from, settled before anything is asked, fetched
	// or written: a pin is recorded only for a bundle this director fetched
	// from a source the cluster declares and saw hash to the digest.
	source, entry, ok := s.pinOrigin(w, r, tenant, body.Coordinate, body.Digest)
	if !ok {
		return
	}
	// An app already installed at another build is being MOVED, and who
	// may move it is the app's update mode's to say (updates.go): under
	// tenant mode whoever may install here, otherwise the cluster's
	// administrator. Asked before anything is fetched or written.
	//
	// So is an app installed at this very build whose move to it the
	// platform rolled back or could not start: installing it again is how
	// that move is asked for once more.
	var retry *rolloutView
	if body.Digest != "" {
		apps, err := s.cfg.Repo.Apps(ctx, tenant)
		if err != nil && !errors.Is(err, gitops.ErrTenantNotFound) {
			s.repoError(w, r, err)
			return
		}
		for _, app := range apps {
			if app.Profile != profile || app.Digest == "" {
				continue
			}
			if app.Digest == body.Digest {
				if retry = s.endedBadly(ctx, tenant, profile, body.Digest); retry == nil {
					continue
				}
			}
			if !s.moveAllowed(w, r, c, tenant, app) {
				return
			}
		}
	}
	catalogueName := ""
	if source != nil {
		catalogueName = source.Name
	}
	if source != nil && entry != profile {
		// The bundle fetched is the coordinate's and the entry written is the
		// path's. Two names would commit one profile and install another.
		s.fail(w, r, http.StatusBadRequest, "the coordinate names a different app than the one being installed")
		return
	}
	if body.DefaultGrant != nil && *body.DefaultGrant {
		target := authz.Tenant(tenant)
		ok, err := s.cfg.Authz.Check(ctx, reqID(ctx), c.user, "can_grant", target)
		if err != nil {
			s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return
		}
		if !ok {
			s.fail(w, r, http.StatusForbidden,
				"installing an app for everyone gives people access to it, which needs can_grant on the tenant; "+
					"install it without defaultGrant, or ask somebody who may grant")
			return
		}
		c.meta.Decision += " and can_grant " + target
	}
	// The profile itself (AD-3).
	//
	// Before the app entry, not after: an entry naming a profile the cluster
	// does not have is a tenant whose app never appears, with the composition
	// failing on a ComponentProfile that is not there. Fetching first means an
	// install either has everything it needs or changed nothing.
	//
	// A request with neither coordinate nor digest fetches nothing and pins
	// nothing: it installs a profile the cluster already holds, or states
	// who an installed app is for. The profile has to be there -- nothing
	// puts profiles on a cluster ahead of an install any more -- and it has
	// to be one this tenant may have.
	//
	// And it may not take an address name the platform depends on
	// (internal/hostnames): asked of the bundle once it is fetched and
	// before it is committed, or of the profile the cluster already holds.
	zone, err := s.reservedAddressZone(ctx)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if source != nil {
		fetched, ok := s.fetchOrStored(w, r, *source, tenant, entry, body.Digest)
		if !ok {
			return
		}
		if s.refusedByFloor(w, r, fetched, "app "+body.Coordinate) {
			return
		}
		if why := fetchedTakesReservedAddress(fetched.Definition, source.origin(tenant), zone); why != "" {
			s.refuseReservedAddress(w, r, tenant, "app "+profile, why)
			return
		}
		res, ok := s.commitEntry(w, r, c, tenant, *source, fetched)
		if !ok {
			return
		}
		if res.Changed {
			s.cfg.Log.InfoContext(ctx, "materialised a catalogue entry",
				"request_id", reqID(ctx), "coordinate", body.Coordinate, "origin", source.origin(tenant), "commit", res.Commit)
		}
	} else {
		if !s.onClusterFor(w, r, tenant, profile, "") {
			return
		}
		why, err := s.onClusterTakesReservedAddress(ctx, profile, zone)
		if err != nil {
			s.repoError(w, r, err)
			return
		}
		if why != "" {
			s.refuseReservedAddress(w, r, tenant, "app "+profile, why)
			return
		}
	}

	res, err := s.cfg.Repo.InstallFrom(ctx, tenant, profile, body.Digest, catalogueName, body.DefaultGrant, c.meta)
	if err != nil || retry == nil {
		s.written(w, r, res, err)
		return
	}
	s.retryMove(w, r, c, tenant, profile, body.Digest, retry)
}

// refusedInPlatformTenant answers an app or an add-on asked for in the
// platform tenant, and reports that it did.
//
// Asked first, before the request is read any further: nothing is fetched
// from a catalogue and no profile is committed for an install that will not
// happen. The caller is already known to hold can_install_app on the tenant,
// so this is not a question of who is asking -- the platform tenant takes
// apps from nobody -- and the answer is 409 with the reason, the same
// sentence whichever client shows it.
func (s *Server) refusedInPlatformTenant(w http.ResponseWriter, r *http.Request, tenant string) bool {
	platform, err := s.cfg.Repo.IsPlatformTenant(r.Context(), tenant)
	if err != nil {
		s.repoError(w, r, err)
		return true
	}
	if platform {
		s.fail(w, r, http.StatusConflict, gitops.ErrPlatformTenant.Error())
		return true
	}
	return false
}

// onClusterFor answers whether a profile named with no coordinate is one
// this tenant can be given: the cluster's catalogue directory holds it, and
// it is not another tenant's own. When it is not, it writes the refusal and
// answers false.
//
// base is the app an add-on is being switched on in, or "" for an app. The
// two refusals differ in what they warn of: an app that is not there never
// appears, and an add-on that is not there is a selection that switches
// nothing on while every screen shows it selected.
//
// One answer for "not there" and for "there, but another tenant's": the
// profile object is visible to anybody who can read the cluster, but this
// service is not where one tenant learns what another published.
func (s *Server) onClusterFor(w http.ResponseWriter, r *http.Request, tenant, name, base string) bool {
	if !gitops.ValidName(name) {
		// Not a profile's name, so not a profile; said as the write says it.
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return false
	}
	have, err := s.cfg.Repo.ProfileOnCluster(r.Context(), name)
	if err != nil {
		s.repoError(w, r, err)
		return false
	}
	if have.Present && profilebundle.UsableBy(have.Origin, tenant) {
		return true
	}
	how := fmt.Sprintf(`give it with its coordinate and digest, {"coordinate": "<catalogue>/%s", "digest": "sha256:<hex>"}, `+
		`so that it is fetched and checked ('kubectl gentian apps list --tenant %s --available' lists both). %s`,
		name, tenant, s.declaredSources(r.Context(), tenant))
	if base != "" {
		s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
			"add-on %s is named without a build, and no profile of that name is on this cluster for this tenant: "+
				"switching it on would activate nothing in %s. Nothing was changed; %s", name, base, how))
		return false
	}
	s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
		"%s is named without a build, and no profile of that name is on this cluster for this tenant: "+
			"profiles are not on a cluster ahead of an install. Nothing was installed; %s", name, how))
	return false
}

// pinOrigin answers which catalogue a coordinate names for this tenant, and
// refuses a request whose build nothing here could verify.
//
// An install comes from a catalogue the tenant sees: one of the cluster's,
// or one of its own. So:
//
//   - A coordinate whose catalogue the tenant does not see is refused --
//     whether nobody declares it or another tenant does, in the same words.
//   - A digest with no coordinate is refused: there is nowhere to fetch the
//     bytes it is the digest of.
//   - A coordinate with no digest is refused, because a fetch from a source
//     is pinned to the build the request names.
//   - Neither is not a pin at all, and answers no source.
//
// It writes the refusal itself and answers false.
func (s *Server) pinOrigin(
	w http.ResponseWriter, r *http.Request, tenant, coordinate, digest string,
) (source *visibleSource, name string, ok bool) {
	ctx := r.Context()
	if coordinate == "" {
		if digest != "" {
			s.fail(w, r, http.StatusUnprocessableEntity,
				"a digest is checked against the bundle its coordinate names, and this request names none: "+
					"state the coordinate, <catalogue>/<app>, with the digest. "+s.declaredSources(ctx, tenant))
			return nil, "", false
		}
		return nil, "", true
	}
	catalogueName, name, cut := strings.Cut(coordinate, "/")
	if !cut || catalogueName == "" || name == "" || strings.Contains(name, "/") {
		s.fail(w, r, http.StatusBadRequest, "coordinate must be <catalogue>/<app>")
		return nil, "", false
	}
	found, known, err := s.resolveSource(ctx, tenant, catalogueName)
	if err != nil {
		s.repoError(w, r, err)
		return nil, "", false
	}
	if !known || s.cfg.Catalogue == nil {
		s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
			"%q is not a catalogue this tenant installs from, so nothing can be fetched from it or verified; "+
				"nothing was installed. %s", catalogueName, s.declaredSources(ctx, tenant)))
		return nil, "", false
	}
	if digest == "" {
		s.fail(w, r, http.StatusBadRequest,
			"installing from a catalogue source needs the entry's digest: sha256:<hex>")
		return nil, "", false
	}
	return &found, name, true
}

// declaredSources says which catalogues a tenant sees, for a refusal that
// has to tell the caller what it could have asked for.
func (s *Server) declaredSources(ctx context.Context, tenant string) string {
	if s.cfg.Catalogue == nil {
		return "This director fetches from no catalogue."
	}
	sources, _, err := s.visibleSources(ctx, tenant)
	if err != nil || len(sources) == 0 {
		return "This tenant has no catalogue to install from."
	}
	names := make([]string, 0, len(sources))
	for _, src := range sources {
		names = append(names, src.Name)
	}
	return "This tenant's catalogues: " + strings.Join(names, ", ") + "."
}

// fetchEntry reads one bundle from its source and checks it against the
// digest. It writes nothing.
//
// The digest comes with the REQUEST. The bytes come from the SOURCE, which is
// not trusted: if they do not hash to that digest the request is refused and
// nothing is written. The request says WHAT, the source says the bytes, and
// only agreement produces an install.
//
// The caller has settled, with pinOrigin, that the tenant sees the catalogue
// and that a digest was stated.
func (s *Server) fetchEntry(
	w http.ResponseWriter, r *http.Request, source visibleSource, name, digest string,
) (*catalogue.Profile, bool) {
	ctx := r.Context()
	coordinate := source.Name + "/" + name
	profile, err := s.cfg.Catalogue.Fetch(ctx, source.Source, name, digest)
	switch {
	case errors.Is(err, catalogue.ErrDigestMismatch):
		// Said plainly and logged, because this is the one failure here that
		// is not a mistake: the source served something other than the build
		// that was asked for.
		s.cfg.Log.ErrorContext(ctx, "a catalogue source served a bundle that is not the build requested",
			"request_id", reqID(ctx), "coordinate", coordinate, "catalogue", source.Key)
		s.fail(w, r, http.StatusBadGateway,
			"the catalogue source served a bundle for "+coordinate+" that is not this entry; nothing was installed")
		return nil, false
	case errors.Is(err, catalogue.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "the catalogue source does not serve "+coordinate)
		return nil, false
	case errors.Is(err, catalogue.ErrAddressRefused):
		// The address was a public one when the catalogue was added and is
		// not one now. Logged with what it was, answered without.
		s.cfg.Log.ErrorContext(ctx, "a catalogue's address is no longer one that is fetched from",
			"request_id", reqID(ctx), "catalogue", source.Key, "error", err.Error())
		s.fail(w, r, http.StatusBadGateway, "the address of catalogue "+source.Name+
			" is not a public https address any more, so nothing is fetched from it; nothing was installed")
		return nil, false
	case errors.Is(err, profilebundle.ErrRefused):
		// The build that was asked for, and not something this cluster
		// applies: a kind a bundle may not hold, an object that is not this
		// profile's, a companion from a tenant's own catalogue. Said with
		// what was found, because whoever publishes the catalogue has to
		// change it; logged, because it is also what an attempt looks like.
		s.cfg.Log.WarnContext(ctx, "a catalogue entry was refused for what its bundle holds",
			"request_id", reqID(ctx), "coordinate", coordinate, "catalogue", source.Key, "error", err.Error())
		s.fail(w, r, http.StatusUnprocessableEntity, "the catalogue entry "+coordinate+
			" is not a bundle this cluster installs: "+refusedFor(err)+"; nothing was installed")
		return nil, false
	case err != nil:
		s.fail(w, r, http.StatusBadGateway, "the catalogue entry could not be read: "+err.Error())
		return nil, false
	}
	return profile, true
}

// fetchOrStored is fetchEntry, except for a build the cluster already holds
// under builds/ of its catalogue directory: that one is read from there and
// not fetched, so a tenant can be moved back to a build the source no longer
// serves. Held to the same digest and the same checks; the source is asked
// only for a build the cluster does not have.
func (s *Server) fetchOrStored(
	w http.ResponseWriter, r *http.Request, source visibleSource, tenant, name, digest string,
) (*catalogue.Profile, bool) {
	stored, found, err := s.cfg.Repo.BuildOnCluster(r.Context(), name, digest)
	if err != nil {
		s.cfg.Log.WarnContext(r.Context(), "a stored build could not be read; fetching instead",
			"request_id", reqID(r.Context()), "profile", name, "digest", digest, "error", err.Error())
	}
	// Only a build this very catalogue brought: one another tenant's own
	// catalogue stored is not this tenant's to install by naming its digest.
	if err == nil && found && stored.Origin == source.origin(tenant) {
		profile, err := catalogue.FromStored(stored.Body, name, digest, source.Key)
		if err == nil {
			return profile, true
		}
		s.cfg.Log.WarnContext(r.Context(), "a stored build is not one this cluster installs; fetching instead",
			"request_id", reqID(r.Context()), "profile", name, "digest", digest, "error", err.Error())
	}
	return s.fetchEntry(w, r, source, name, digest)
}

// refusedByFloor refuses a fetched build whose spec.requires.platform is
// above this platform's version line, and reports that it did. Asked of
// the bundle itself, after the digest and before anything is written: the
// index repeats the floor so a listing can say it, but what an install is
// held to is what the build states.
func (s *Server) refusedByFloor(w http.ResponseWriter, r *http.Request, fetched *catalogue.Profile, what string) bool {
	floor := ""
	if fetched.Definition != nil && fetched.Definition.Spec.Requires != nil {
		floor = fetched.Definition.Spec.Requires.Platform
	}
	unmet := platformversion.Unmet(floor)
	if unmet == "" {
		return false
	}
	s.fail(w, r, http.StatusUnprocessableEntity, what+" "+unmet+
		"; move the platform forward first, or install a build whose floor this cluster meets. Nothing was installed")
	return true
}

// commitEntry commits a fetched and verified bundle beside its profile,
// recorded as coming from the catalogue it was fetched from.
//
// ComponentProfiles are cluster-scoped, so a name is one profile for every
// tenant. A profile from a tenant's own catalogue never takes a name another
// origin holds, and nothing takes a name a tenant's own catalogue holds: the
// install is refused, with who has to rename.
func (s *Server) commitEntry(
	w http.ResponseWriter, r *http.Request, c call, tenant string, source visibleSource, profile *catalogue.Profile,
) (gitops.Result, bool) {
	res, err := s.cfg.Repo.MaterialiseProfile(r.Context(), profile.Name, profile.Digest, profile.Body, source.origin(tenant), c.meta)
	var taken *gitops.ErrProfileNameTaken
	switch {
	case errors.Is(err, gitops.ErrBundleTooLarge):
		// Refused rather than installed unverifiable: the operator checks a
		// pinned install against the bundle, and one it cannot be given
		// would be held at rollout for good.
		s.fail(w, r, http.StatusUnprocessableEntity,
			"the catalogue entry "+profile.Name+" is too large to be installed at a digest; nothing was installed")
		return gitops.Result{}, false
	case errors.Is(err, gitops.ErrProfileStatesOrigin):
		s.fail(w, r, http.StatusUnprocessableEntity, "the catalogue entry "+profile.Name+
			" carries an annotation only the platform writes (its bundle or its origin); nothing was installed")
		return gitops.Result{}, false
	case errors.Is(err, profilebundle.ErrRefused):
		s.fail(w, r, http.StatusUnprocessableEntity, "the catalogue entry "+profile.Name+
			" is not a bundle this cluster installs: "+refusedFor(err)+"; nothing was installed")
		return gitops.Result{}, false
	case errors.As(err, &taken):
		s.cfg.Log.WarnContext(r.Context(), "a profile's name is taken by a profile of another origin",
			"request_id", reqID(r.Context()), "profile", taken.Name, "origin", taken.Origin, "holder", taken.Holder)
		s.fail(w, r, http.StatusConflict, nameTakenMessage(taken, tenant, source))
		return gitops.Result{}, false
	case err != nil:
		s.repoError(w, r, err)
		return gitops.Result{}, false
	}
	return res, true
}

// refusedFor is what a refused bundle was found to hold, without the words
// every such refusal begins with.
func refusedFor(err error) string {
	_, found, cut := strings.Cut(err.Error(), profilebundle.ErrRefused.Error()+": ")
	if !cut {
		return err.Error()
	}
	return found
}

// nameTakenMessage says that a profile's name is taken and who has to rename.
// It names no other tenant: whose the other profile is, is in the log.
func nameTakenMessage(taken *gitops.ErrProfileNameTaken, tenant string, source visibleSource) string {
	name := taken.Name
	suggestion := fmt.Sprintf("publish it as %s-%s in the catalogue %s and install that", tenant, name, source.Name)
	switch {
	case taken.Platform:
		if source.scope == scopeTenant {
			return fmt.Sprintf("the name %s is taken on this cluster by a component the platform ships, and a profile is "+
				"one object for the whole cluster. Nothing was installed; %s", name, suggestion)
		}
		return fmt.Sprintf("the name %s is taken on this cluster by a component the platform ships. Nothing was installed; "+
			"the catalogue %s has to publish its entry under another name", name, source.Name)
	case source.scope == scopeTenant:
		return fmt.Sprintf("the name %s is already taken on this cluster by a profile from another catalogue, and a profile "+
			"is one object for the whole cluster. Nothing was installed; %s", name, suggestion)
	default:
		return fmt.Sprintf("the name %s is already taken on this cluster by a profile from a tenant's own catalogue, so "+
			"the entry of the cluster's catalogue %s cannot be installed under it. Nothing was installed. The tenant that "+
			"published it has to rename its profile (<tenant>-%s) and install that instead; the cluster's administrator "+
			"can see whose it is", name, source.Name, name)
	}
}

// uninstallNote is said with every uninstall that took an app away. The
// app's data stays, and a backup taken afterwards holds it, as an
// uninstalled app's; deleting the tenant, or purging the app, destroys it.
const uninstallNote = "The app's data is kept, and backups taken from now on go on including it, as an uninstalled app's. " +
	"Deleting the tenant, or purging the app, destroys it."

func (s *Server) uninstall(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.Uninstall(r.Context(), r.PathValue("t"), r.PathValue("p"), c.meta)
	if err != nil || !res.Changed {
		s.written(w, r, res, err)
		return
	}
	s.json(w, http.StatusAccepted, map[string]any{"status": res.Status, "commit": res.Commit, "note": uninstallNote})
}

type addonsRequest struct {
	Addons []addonEntry `json:"addons"`
}

// addonEntry is one addon of the selection: a name, or the build to install
// it at.
//
// A string is the addon's name, as it has always been. An object is
// {"coordinate": "<catalogue>/<addon>", "digest": "sha256:<hex>"} -- an item
// of the store's confirmation, sent on -- and the addon it names is the
// second half of the coordinate.
type addonEntry struct {
	Name       string
	Coordinate string
	Digest     string
}

func (a *addonEntry) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '"' {
		return json.Unmarshal(raw, &a.Name)
	}
	var pinned struct {
		Coordinate string `json:"coordinate"`
		Digest     string `json:"digest"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pinned); err != nil {
		return err
	}
	a.Coordinate, a.Digest = pinned.Coordinate, pinned.Digest
	return nil
}

// setAddons commits which addons are activated inside an installed app, and
// the build of each that is pinned.
//
// A pinned addon is installed with the guarantees of a pinned app, by the
// same steps: its coordinate must name a catalogue source the cluster
// declares, its bundle is fetched from there and checked against the digest,
// the verified bundle is committed beside the profile, and only then is the
// pin written on the entry. Every bundle is fetched and checked before the
// first is committed, so a selection one of whose builds does not verify
// changes nothing.
//
// A pinned addon is accepted only for an app whose own entry carries a
// digest. Names inside an app that carries none are set as before.
func (s *Server) setAddons(w http.ResponseWriter, r *http.Request, c call) {
	ctx := r.Context()
	tenant, profile := r.PathValue("t"), r.PathValue("p")
	if s.refusedInPlatformTenant(w, r, tenant) {
		return
	}
	var body addonsRequest
	if err := decode(r, &body); err != nil || body.Addons == nil {
		s.fail(w, r, http.StatusBadRequest,
			`body must be {"addons": [...]}, each a name or {"coordinate": "<catalogue>/<addon>", "digest": "sha256:<hex>"}`)
		return
	}
	names := make([]string, 0, len(body.Addons))
	seen := map[string]bool{}
	var pinned []addonEntry
	var sources []visibleSource
	var bare []string
	var pins []gitops.AddonPin
	for _, entry := range body.Addons {
		name := entry.Name
		if entry.Coordinate != "" || entry.Digest != "" {
			if entry.Digest != "" {
				digest, err := catalogue.CanonicalDigest(entry.Digest)
				if err != nil {
					s.fail(w, r, http.StatusBadRequest, "digest must be sha256:<64 hex characters>")
					return
				}
				entry.Digest = digest
			}
			// The rule an app's pin is under: a declared source, and a
			// digest only beside the coordinate it is the digest of.
			from, addon, ok := s.pinOrigin(w, r, tenant, entry.Coordinate, entry.Digest)
			if !ok {
				return
			}
			if from == nil {
				// An object with neither: not a build, and not a name.
				s.fail(w, r, http.StatusBadRequest,
					`an addon is a name or {"coordinate": "<catalogue>/<addon>", "digest": "sha256:<hex>"}`)
				return
			}
			name = addon
			entry.Name = addon
			pinned = append(pinned, entry)
			sources = append(sources, *from)
			pins = append(pins, gitops.AddonPin{Name: addon, Digest: entry.Digest, Catalogue: from.Name})
		} else if name != "" {
			bare = append(bare, name)
		}
		if name == "" {
			s.fail(w, r, http.StatusBadRequest,
				`an addon is a name or {"coordinate": "<catalogue>/<addon>", "digest": "sha256:<hex>"}`)
			return
		}
		if seen[name] {
			s.fail(w, r, http.StatusBadRequest, "addon "+name+" is listed twice")
			return
		}
		seen[name] = true
		names = append(names, name)
	}

	// An add-on named with no build is one whose profile the cluster must
	// already hold. Nothing puts profiles on a cluster ahead of an install,
	// so a name that matches none is refused here: written to the manifest
	// it would be a selection every screen shows and nothing acts on.
	//
	// And an add-on is under the rule an app is under: no entry of its own
	// on an address name the platform depends on.
	zone, err := s.reservedAddressZone(ctx)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	for _, name := range bare {
		if !s.onClusterFor(w, r, tenant, name, profile) {
			return
		}
		why, err := s.onClusterTakesReservedAddress(ctx, name, zone)
		if err != nil {
			s.repoError(w, r, err)
			return
		}
		if why != "" {
			s.refuseReservedAddress(w, r, tenant, "add-on "+name, why)
			return
		}
	}

	if len(pinned) > 0 {
		// Nothing is fetched or committed for an app the tenant does not
		// have: the answer is the one the write itself would give.
		apps, err := s.cfg.Repo.Apps(ctx, tenant)
		if err != nil {
			s.repoError(w, r, err)
			return
		}
		installed, basePinned := false, false
		for _, a := range apps {
			if a.Profile == profile {
				installed, basePinned = true, a.Digest != ""
			}
		}
		if !installed {
			s.written(w, r, gitops.Result{Status: "not_installed"}, nil)
			return
		}
		// An addon is pinned only inside a pinned app. An addon takes effect
		// in the release of its base, so a stated build of an addon inside a
		// base at no stated build pins half of what runs; and the license
		// report lists an addon under its app, which it does not list
		// without a digest. Refused before anything is fetched.
		if !basePinned {
			s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
				"an addon is installed at a stated build only inside an app that is: %s is installed "+
					"with no digest. Install %s at a stated build first -- "+
					"`kubectl gentian apps install %s --tenant %s` pins it -- and set its addons again. "+
					"Addons given by name need no pin. Nothing was fetched or written.",
				profile, profile, profile, tenant))
			return
		}
		bundles := make([]*catalogue.Profile, 0, len(pinned))
		for i, entry := range pinned {
			bundle, ok := s.fetchEntry(w, r, sources[i], entry.Name, entry.Digest)
			if !ok {
				return
			}
			if s.refusedByFloor(w, r, bundle, "add-on "+entry.Coordinate) {
				return
			}
			if why := fetchedTakesReservedAddress(bundle.Definition, sources[i].origin(tenant), zone); why != "" {
				s.refuseReservedAddress(w, r, tenant, "add-on "+entry.Name, why)
				return
			}
			bundles = append(bundles, bundle)
		}
		for i, bundle := range bundles {
			res, ok := s.commitEntry(w, r, c, tenant, sources[i], bundle)
			if !ok {
				return
			}
			if res.Changed {
				s.cfg.Log.InfoContext(ctx, "materialised a catalogue entry",
					"request_id", reqID(ctx), "coordinate", pinned[i].Coordinate, "commit", res.Commit)
			}
		}
	}

	res, err := s.cfg.Repo.SetAddonsPinned(ctx, tenant, profile, names, pins, c.meta)
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
//
// The answer also says which platform this is: its version line, the one a
// bundle is measured against on import, and the commit the image was built
// from when the build recorded one. The director ships in the operator's
// image, so its own build is the operator's.
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
		if c.List {
			entry["list"] = true
		}
		// A setting the installer reads: the commit changes nothing on the
		// cluster until that step runs again, and a console has to say so.
		if c.InstallStep != "" {
			entry["installStep"] = c.InstallStep
		}
		if v, ok := values[c.Path]; ok {
			entry["value"] = v
		}
		out = append(out, entry)
	}
	answer := map[string]any{
		"cluster":             s.cfg.Cluster,
		"settings":            out,
		"platformVersionLine": backup.PlatformVersionLine,
	}
	if commit := buildCommit(); commit != "" {
		answer["build"] = commit
	}
	s.json(w, http.StatusOK, answer)
}

// buildCommit is the commit this binary was built from, or "" when the build
// recorded none (sources built without their git history).
func buildCommit() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}
	// An image built without the repository's history carries no revision;
	// the chart then names the build by the image tag it renders.
	return os.Getenv("GENTIAN_BUILD")
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
		s.fail(w, r, http.StatusBadRequest, `body must be {"name": "<name>", "displayName": "<name>"}, optionally with `+
			`"requireMFA": false, "perimeter": {"adminsApprove": true|false}, "catalogue": {"delegated": true}, "signUp": true, "country": "CH", `+
			`"brokeredTo": [{"name": "<name>", "redirectURI": "https://…", "jwksURL": "https://…"}]`)
		return
	}
	for _, b := range body.BrokeredTo {
		if err := gitops.ValidBrokeredTo(b); err != nil {
			s.fail(w, r, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !gitops.ValidName(body.Name) {
		s.fail(w, r, http.StatusBadRequest,
			"a tenant name is a DNS label: lower-case letters, digits and hyphens, starting and ending with a letter or digit")
		return
	}
	country, err := gitops.NormaliseCountry(body.Country)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	body.Country = country
	// Whether its administrators approve its public addresses is the
	// request's to say. Unsaid it is off, except for the user tenant of a
	// single-tenancy cluster (gitops.AdminsApproveAtCreation). Settled here
	// and written as settled, so that the answer states what the manifest
	// holds.
	adminsApprove, err := s.cfg.Repo.AdminsApproveAtCreation(r.Context(), body)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	body.Perimeter = &gitops.NewTenantPerimeter{AdminsApprove: &adminsApprove}
	res, err := s.cfg.Repo.CreateTenant(r.Context(), body, c.meta)
	if errors.Is(err, gitops.ErrTenantExists) {
		s.fail(w, r, http.StatusConflict, "a tenant of that name already exists")
		return
	}
	if errors.Is(err, gitops.ErrSingleTenancy) || errors.Is(err, gitops.ErrNameIsAlias) {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	answer := map[string]any{"status": res.Status, "adminsApprove": adminsApprove, "signUp": body.SignUp, "brokeredTo": len(body.BrokeredTo), "country": body.Country}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	s.json(w, http.StatusAccepted, answer)
}

// clusterBranding answers the cluster's brand; an empty one when it sets
// none and the pages show the platform's own.
func (s *Server) clusterBranding(w http.ResponseWriter, r *http.Request, _ call) {
	spec, found, err := s.cfg.Repo.ClusterBranding(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if !found {
		spec = &gentianov1alpha1.BrandingSpec{}
	}
	s.json(w, http.StatusOK, map[string]any{"set": found, "branding": spec})
}

// setClusterBranding commits the cluster's brand, refused when the pages
// could not show it.
func (s *Server) setClusterBranding(w http.ResponseWriter, r *http.Request, c call) {
	var spec gentianov1alpha1.BrandingSpec
	if err := decode(r, &spec); err != nil {
		s.fail(w, r, http.StatusBadRequest, "body must be a Branding spec: identity, tokens")
		return
	}
	res, err := s.cfg.Repo.SetClusterBranding(r.Context(), spec, c.meta)
	if errors.Is(err, gitops.ErrInvalidBranding) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// setTenantDomain binds a tenant to a custom domain: one commit of its
// TenantDomain, which the operator acts on.
func (s *Server) setTenantDomain(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Domain string `json:"domain"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Domain) == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"domain": "<hostname>"}; DELETE removes one`)
		return
	}
	s.writeTenantDomain(w, r, c, body.Domain)
}

// clearTenantDomain puts the tenant back on its default address. It is
// refused to no tenant, under either tenancy mode.
func (s *Server) clearTenantDomain(w http.ResponseWriter, r *http.Request, c call) {
	s.writeTenantDomain(w, r, c, "")
}

func (s *Server) writeTenantDomain(w http.ResponseWriter, r *http.Request, c call, domain string) {
	res, err := s.cfg.Repo.SetTenantDomain(r.Context(), r.PathValue("t"), domain, c.meta)
	// The three refusals of a bind, each worded for a person: the domain
	// itself, the platform tenant, and a single-tenancy cluster.
	if errors.Is(err, gitops.ErrInvalidDomain) || errors.Is(err, gitops.ErrPlatformTenantDomain) ||
		errors.Is(err, gitops.ErrSingleTenancy) {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// setExposureAlias gives a published surface a second address directly under
// the cluster's domain: one commit of the alias on the surface's own entry.
func (s *Server) setExposureAlias(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Host string `json:"host"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Host) == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"host": "<name>.<the cluster's domain>"}; DELETE removes the alias`)
		return
	}
	s.writeExposureAlias(w, r, c, body.Host)
}

// clearExposureAlias takes the second address away. The surface stays at its
// own.
func (s *Server) clearExposureAlias(w http.ResponseWriter, r *http.Request, c call) {
	s.writeExposureAlias(w, r, c, "")
}

func (s *Server) writeExposureAlias(w http.ResponseWriter, r *http.Request, c call, host string) {
	res, err := s.cfg.Repo.SetExposureAlias(r.Context(), r.PathValue("t"), r.PathValue("inst"), r.PathValue("name"), host, c.meta)
	switch {
	case errors.Is(err, gitops.ErrAliasInvalid):
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, gitops.ErrAliasHeld), errors.Is(err, gitops.ErrNotPublished):
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// retireTenant stops git describing a tenant, which is what removes it.
//
// Whether its data goes with it is the manifest's deletionPolicy, honoured by
// the operator, not something decided here.
func (s *Server) retireTenant(w http.ResponseWriter, r *http.Request, c call) {
	tenant := r.PathValue("t")
	res, err := s.cfg.Repo.RetireTenant(r.Context(), tenant, c.meta)
	if errors.Is(err, gitops.ErrTenantProtected) {
		s.fail(w, r, http.StatusForbidden, err.Error())
		return
	}
	if errors.Is(err, gitops.ErrTenantNotFound) && s.cfg.Lifecycle != nil {
		s.retireOrphan(w, r, c, tenant, err)
		return
	}
	s.written(w, r, res, err)
}

// retireOrphan is a retire of a tenant git does not describe. The cluster
// may still run it: an Argo CD sync that finished after the tenant's
// directory was removed re-creates the Tenant with nothing owning it, and
// "tenant not found" was then the answer to the one command that should
// have removed it. The operator deletes exactly such an object
// (applifecycle.RemoveOrphanTenant) and nothing else; a Tenant the cluster
// does not hold either is the not-found it always was.
func (s *Server) retireOrphan(w http.ResponseWriter, r *http.Request, c call, tenant string, notFound error) {
	status, body, err := s.cfg.Lifecycle.Do(r.Context(), "/v1/tenants/"+tenant+"/actions/remove-orphan", c.meta.ActorName(), nil)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, fmt.Sprintf("%s in git; asking the cluster whether it still runs it: %v", notFound, err))
		return
	}
	switch status {
	case http.StatusAccepted:
		var removed struct {
			Application     string `json:"application"`
			AlreadyDeleting bool   `json:"alreadyDeleting"`
		}
		_ = json.Unmarshal(body, &removed)
		detail := fmt.Sprintf("%s was not in git, but the cluster still ran it: its Application %s had been pruned while a sync was finishing, which re-created the Tenant with no owner. It is being deleted now, under its deletionPolicy.", tenant, removed.Application)
		if removed.AlreadyDeleting {
			detail = fmt.Sprintf("%s was not in git; the cluster is already deleting it.", tenant)
		}
		s.json(w, http.StatusAccepted, map[string]any{"status": "orphan-removed", "detail": detail})
	case http.StatusNotFound:
		s.fail(w, r, http.StatusNotFound, notFound.Error())
	default:
		s.fail(w, r, status, fmt.Sprintf("%s in git, and the cluster refused to remove it: %s", notFound, lifecycle.ErrorMessage(body)))
	}
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
	if errors.Is(err, gitops.ErrSingleTenancy) || errors.Is(err, gitops.ErrSingleRefused) {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	// A change to a setting the installer reads lands in git like any other
	// and reaches the cluster only when that step runs again: the answer
	// names the step, so the caller is not left waiting for Argo CD.
	if err == nil && res.Changed {
		if step := installStepOf(body.Settings); step != "" {
			s.json(w, http.StatusAccepted, map[string]any{"status": res.Status, "commit": res.Commit, "installStep": step})
			return
		}
	}
	s.written(w, r, res, err)
}

// installStepOf is the installer step the settings named need, if any of
// them is read by the installer rather than the operator.
func installStepOf(settings map[string]string) string {
	for _, c := range gitops.ClusterSettings() {
		if _, named := settings[c.Path]; named && c.InstallStep != "" {
			return c.InstallStep
		}
	}
	return ""
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
	var refusal *schemacheck.Refusal
	switch {
	case errors.As(err, &refusal):
		// 503: the request was fine and nothing was written. The cluster's
		// resource definitions are older than this software and would drop
		// part of the change when Argo CD applied it -- or that could not be
		// ruled out. The message names the definition, the fields and what
		// to do; it holds nothing of git's, so it goes to the caller whole.
		s.cfg.Log.WarnContext(r.Context(), "write refused: the cluster's definitions would drop part of it",
			"request_id", reqID(r.Context()), "unconfirmed", refusal.Unconfirmed,
			"kind", refusal.Kind, "fields", refusal.Fields)
		if refusal.Unconfirmed {
			w.Header().Set("Retry-After", "15")
		}
		s.fail(w, r, http.StatusServiceUnavailable, refusal.Message)
	case errors.Is(err, gitops.ErrTenantNotFound):
		s.fail(w, r, http.StatusNotFound, "tenant not found")
	case errors.Is(err, gitops.ErrPlatformTenant):
		s.fail(w, r, http.StatusConflict, gitops.ErrPlatformTenant.Error())
	case errors.Is(err, gitops.ErrNoClusterClaim):
		s.fail(w, r, http.StatusNotFound, "this cluster has no Cluster claim in the repository")
	case errors.Is(err, gitops.ErrInvalidName):
		s.fail(w, r, http.StatusBadRequest, "invalid name")
	case errors.Is(err, gitops.ErrInvalidBackupPolicy):
		s.fail(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, gitops.ErrNoPushCredential):
		// 503, not 500: nothing is broken and the request was fine. The
		// cluster has not been given a credential to write with, which is
		// something an operator fixes and a caller can do nothing about.
		s.cfg.Log.ErrorContext(r.Context(), "no credential to push with",
			"request_id", reqID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusServiceUnavailable,
			"this cluster has no credential to write to its deployments repository: "+
				"the change was prepared and could not be pushed. Supply the token "+
				"(GENTIAN_DEPLOYMENTS_GIT_TOKEN) and apply the deployments Repository claim.")
	case errors.Is(err, gitops.ErrNoSigningKey):
		// 503 for the same reason: the request was fine, and the key is
		// something the vault and External Secrets owe this process.
		s.cfg.Log.ErrorContext(r.Context(), "no key to sign with",
			"request_id", reqID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusServiceUnavailable,
			"this director has no signing key yet, and Argo CD syncs only signed commits: "+
				"nothing was pushed. Check the ExternalSecret gentian-director-signing.")
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
