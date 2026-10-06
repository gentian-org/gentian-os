/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package registrar keeps the list of people: it invites them, puts them in
// groups and removes them, at Keycloak, for the person entitled to ask
// (operator-split-plan.md §3.10).
//
// It is a process of its own for the reason the director and the custodian
// are. The director holds the credential that pushes to git; while it also
// held a Keycloak credential for every realm, one process could rewrite what
// the cluster runs and who may sign in to it. The registrar holds the
// Keycloak credential and nothing else that stands: no git, no vault, and in
// the cluster only the right to read the tenants and the Cluster claim.
//
// Every route follows the director's four steps and none may skip one:
// establish who is calling from a verified token, name the relation the route
// requires, ask OpenFGA, and only then act. A route is registered together
// with its relation, so a handler that forgot to authorise is not something
// this package can express. The paths, the relations and the bodies are the
// ones the director served; a caller changes the address it calls and
// nothing else.
//
// One thing it will not do for anybody: change who holds a platform role.
// That rule lives in internal/registrar/identity/guard.go, on the way every
// write leaves.
package registrar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
	"github.com/gentian-org/gentian-os/internal/registrar/record"
)

// Authenticator establishes the caller's identity from a request.
type Authenticator interface {
	FromRequest(r *http.Request) (*authn.Identity, error)
}

// Identity is the part of the Keycloak client this API uses. An interface so
// the routes can be tested without a realm, and so nothing here can reach a
// method that was not meant to be reachable from a request.
type Identity interface {
	Realm(name string) (identity.Realm, error)
	People(ctx context.Context, r identity.Realm, search string, limit int) ([]identity.Person, error)
	Person(ctx context.Context, r identity.Realm, id string) (identity.Person, error)
	Groups(ctx context.Context, r identity.Realm) ([]identity.Group, error)
	Invite(ctx context.Context, r identity.Realm, inv identity.Invitation) (identity.Person, error)
	SetMembership(ctx context.Context, r identity.Realm, userID, groupPath string, member bool) error
	PasswordPolicy(ctx context.Context, r identity.Realm) (string, error)
	SetPasswordPolicy(ctx context.Context, r identity.Realm, policy string) error
	ZoneLanding(ctx context.Context, r identity.Realm, clientID string) string
	SendPasswordReset(ctx context.Context, r identity.Realm, userID, clientID, redirectURI string) error
	UpdatePerson(ctx context.Context, r identity.Realm, id string, u identity.PersonUpdate) (identity.Person, error)
	RemovePerson(ctx context.Context, r identity.Realm, id string) error
	RequireTOTP(ctx context.Context, r identity.Realm, id string, mail bool, clientID, redirectURI string) error
	RemoveTOTP(ctx context.Context, r identity.Realm, id string) error
	CreateGroup(ctx context.Context, r identity.Realm, path string) (identity.Group, error)
	DeleteGroup(ctx context.Context, r identity.Realm, path string) error
	RenameGroup(ctx context.Context, r identity.Realm, path, newPath string) (identity.Group, error)
	FindUser(ctx context.Context, r identity.Realm, username string) (identity.Person, error)
	ActivateAccount(ctx context.Context, r identity.Realm, id, email string, requireMFA bool, clientID, redirectURI string) (identity.Activation, error)
	GroupMembers(ctx context.Context, r identity.Realm, path string) ([]identity.Person, error)
	UserCount(ctx context.Context, r identity.Realm) (int, error)
}

// Config assembles a Server.
type Config struct {
	Authn Authenticator
	Authz authz.Checker
	// Tenants answers which realm a tenant's people live in. The director
	// read that from git; the registrar has no git and reads the cluster.
	Tenants Tenants
	// Identity is how the registrar speaks for Keycloak. It holds one
	// credential per realm and can name no other, so what it can reach is
	// decided by what the operator handed it rather than by what a handler
	// remembered to check.
	Identity Identity
	// Record is the durable record of who was allowed to ask for a change to
	// a person. Optional: a cluster whose database has not been provisioned
	// keeps the log line and nothing else, which is a worse record rather
	// than a registrar that does not start.
	Record *record.Store
	Log    *slog.Logger
	// Cluster is the id of the one cluster this registrar serves: the object
	// cluster verbs are checked against, and the only {c} the routes accept.
	Cluster string
	// InviteClientID overrides the client an invitation link names. Empty
	// derives it from the realm, which is right on every cluster but one --
	// see inviteClientID. InviteRedirectURI is where the link lands, and is
	// empty to read it off the zone client.
	InviteClientID    string
	InviteRedirectURI string
	// DesktopAPI is where a tenant's desktop API answers, with %s for the
	// tenant: the settings templates live there, and the registrar relays
	// the caller's own token to them. Empty means templates are not offered.
	DesktopAPI string
}

// Server is the registrar's API.
type Server struct {
	cfg Config
	mux *http.ServeMux
	// Addr is what Start listens on.
	Addr string
}

// New returns a Server with every route registered.
func New(cfg Config) (*Server, error) {
	if cfg.Authn == nil || cfg.Authz == nil || cfg.Tenants == nil || cfg.Identity == nil {
		return nil, errors.New("registrar: authenticator, checker, tenants and identity are all required")
	}
	if cfg.Cluster == "" {
		return nil, errors.New("registrar: the cluster id is required: it is what cluster verbs are asked about")
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

// Start serves until ctx is cancelled. It satisfies manager.Runnable, so the
// server shares the lifecycle of the client it reads the tenants with.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type ctxRequestID struct{}

// inboundID is what a request id from the gateway must look like to be kept.
// It ends up in Keycloak's admin event and in the record, so it is matched,
// not escaped.
var inboundID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// requestID keeps the gateway's id so the audit logs join on one value
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
	// subject is the caller's Keycloak subject.
	subject string
	// decision is the relation and object that permitted the call, in the
	// form the director's commit trailer uses: "can_manage_users tenant:demo".
	decision string
}

// object names what a route's relation is checked against.
type object func(r *http.Request) (string, error)

var errInvalidName = errors.New("invalid name")

// dnsLabel is what a tenant name must be, as everywhere else on the platform.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// clusterObject accepts only this registrar's own cluster. Another id is not
// forbidden, it does not exist here.
func (s *Server) clusterObject(r *http.Request) (string, error) {
	if c := r.PathValue("c"); c != s.cfg.Cluster || c == "" {
		return "", errInvalidName
	}
	return authz.Cluster(s.cfg.Cluster), nil
}

func tenantObject(r *http.Request) (string, error) {
	t := r.PathValue("t")
	if !dnsLabel.MatchString(t) {
		return "", errInvalidName
	}
	return authz.Tenant(t), nil
}

// authorize runs the steps every route shares: who is calling, may they do
// relation to the route's object. On refusal it has already answered, and ok
// is false. A store that does not answer is a refusal.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, pattern, relation string, obj object) (c call, ok bool) {
	ctx := r.Context()
	ident, err := s.cfg.Authn.FromRequest(r)
	if err != nil {
		s.cfg.Log.WarnContext(ctx, "authentication failed", "request_id", reqID(ctx), "reason", err.Error(), "route", pattern)
		w.Header().Set("WWW-Authenticate", `Bearer realm="gentian-registrar"`)
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
	return call{subject: user[len("user:"):], decision: relation + " " + target}, true
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

// action registers something that happens once. The pattern must be a POST
// under `/actions/`, which is checked here rather than trusted: a route that
// looked like a read and made something happen would be the one mistake this
// distinction exists to prevent.
func (s *Server) action(pattern, relation string, obj object, h func(http.ResponseWriter, *http.Request, call)) {
	if !strings.HasPrefix(pattern, "POST ") || !strings.Contains(pattern, "/actions/") {
		panic("registrar: an action must be a POST under /actions/: " + pattern)
	}
	s.guarded(pattern, relation, obj, h)
}

// routes registers what the registrar serves: people, groups and the realm's
// settings.
//
// Every write is an ACTION and none is a PUT. A commit says what should be
// true from now on and is reviewable in git for ever; inviting somebody
// happens once, and people do not belong in an append-only history.
// can_manage_users throughout, except the password policy, which is a
// statement about the tenant rather than about a person and sits with
// can_set_policy.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	s.guarded("GET /v1/tenants/{t}/people", "can_manage_users", tenantObject, s.listPeople)
	s.guarded("GET /v1/tenants/{t}/people/{id}", "can_manage_users", tenantObject, s.getPerson)
	s.guarded("GET /v1/tenants/{t}/groups", "can_manage_users", tenantObject, s.listGroups)
	s.guarded("GET /v1/tenants/{t}/identity", "can_manage_users", tenantObject, s.tenantIdentitySettings)

	s.action("POST /v1/tenants/{t}/actions/invite-person", "can_manage_users", tenantObject, s.invitePerson)
	s.action("POST /v1/tenants/{t}/actions/set-membership", "can_manage_users", tenantObject, s.setMembership)
	s.action("POST /v1/tenants/{t}/actions/send-password-reset", "can_manage_users", tenantObject, s.sendPasswordReset)
	s.action("POST /v1/tenants/{t}/actions/set-password-policy", "can_set_policy", tenantObject, s.setPasswordPolicy)

	// Editing somebody, and the groups they are put in. can_manage_users
	// throughout, like the invitation they extend.
	s.guarded("GET /v1/tenants/{t}/group-members", "can_manage_users", tenantObject, s.listGroupMembers)
	s.guarded("GET /v1/tenants/{t}/templates", "can_manage_users", tenantObject, s.listTemplates)
	s.action("POST /v1/tenants/{t}/actions/update-person", "can_manage_users", tenantObject, s.updatePerson)
	s.action("POST /v1/tenants/{t}/actions/remove-person", "can_manage_users", tenantObject, s.removePerson)
	s.action("POST /v1/tenants/{t}/actions/require-totp", "can_manage_users", tenantObject, s.requireTOTP)
	s.action("POST /v1/tenants/{t}/actions/remove-totp", "can_manage_users", tenantObject, s.removeTOTP)
	s.action("POST /v1/tenants/{t}/actions/create-group", "can_manage_users", tenantObject, s.createGroup)
	s.action("POST /v1/tenants/{t}/actions/delete-group", "can_manage_users", tenantObject, s.deleteGroup)
	s.action("POST /v1/tenants/{t}/actions/rename-group", "can_manage_users", tenantObject, s.renameGroup)

	// Handing a tenant's administrator account to its holder: whoever may
	// bring tenants on (can_configure on the cluster) issues the link.
	s.action("POST /v1/clusters/{c}/tenants/{t}/actions/activate-admin", "can_configure", s.clusterObject, s.activateAdmin)

	// How many people hold an account on this cluster. A count and no
	// names, so it is can_audit like every other read of the cluster.
	s.guarded("GET /v1/clusters/{c}/people/count", "can_audit", s.clusterObject, s.clusterUserCount)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.json(w, code, map[string]any{"error": msg, "request_id": reqID(r.Context())})
}

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
