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

package authz

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// Verifier checks a bearer token against the platform's issuer.
type Verifier interface {
	Verify(ctx context.Context, raw string) (*authn.Identity, error)
}

// Store is the part of the authorization store L2 needs: a decision, and the
// changelog that says when a decision may have changed.
type Store interface {
	Check(ctx context.Context, requestID, user, relation, object string) (bool, error)
	Changes(ctx context.Context, objectType, token string) ([]authz.Change, string, error)
}

// Request is what the Gateway tells the shim about a request.
type Request struct {
	ID            string
	Host          string
	Path          string
	Authorization string
	Cookies       map[string]string
}

// Decision is the shim's answer.
type Decision struct {
	Allow bool
	// Identified is false when the request passed with no identity: an oidc
	// route with no valid session, left to the OIDC filter behind the shim.
	Identified bool
	// Status is the HTTP status to answer with when not allowed.
	Status int
	Reason string
	// Headers are set on the request to the backend, overriding what the
	// client sent; RemoveHeaders are stripped.
	Headers       map[string]string
	RemoveHeaders []string
}

type identity struct {
	subject, realm, session, email, name string
}

// Decider answers L2.
type Decider struct {
	verify Verifier
	store  Store
	cache  *cache
	log    *slog.Logger
	now    func() time.Time

	mu    sync.RWMutex
	table *Table
}

// Options configure a Decider.
type Options struct {
	Verifier Verifier
	Store    Store
	Table    *Table
	// CacheTTL bounds a cached allow; eviction on the changelog is what
	// normally retires one.
	CacheTTL time.Duration
	Logger   *slog.Logger
	Now      func() time.Time
}

// New returns a Decider.
func New(o Options) *Decider {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = 5 * time.Minute
	}
	return &Decider{verify: o.Verifier, store: o.Store, cache: newCache(o.CacheTTL, o.Now), log: o.Logger, now: o.Now, table: o.Table}
}

// SetTable replaces the route table.
func (d *Decider) SetTable(t *Table) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.table = t
}

// Table returns the current route table.
func (d *Decider) Table() *Table {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.table
}

// Evict forgets every cached decision: the changelog moved.
func (d *Decider) Evict() int { return d.cache.evictAll() }

// Identity headers the backend receives. The gateway strips whatever the
// client sent under these names by overriding them, so the app may trust
// one at all (networking.md §2, L1').
const (
	HeaderSubject = "x-gentian-subject"
	HeaderRealm   = "x-gentian-realm"
	HeaderSession = "x-gentian-session"
	HeaderEmail   = "x-gentian-email"
	HeaderName    = "x-gentian-name"
)

// Decide answers whether the request may reach its route.
//
// Fail closed, cached allows carry (networking.md §4): a store that cannot
// be reached leaves decisions already cached valid until they expire and
// answers 503 to everything else. Nothing not previously allowed gets through.
func (d *Decider) Decide(ctx context.Context, req Request) Decision {
	route := d.Table().Match(req.Host)
	if route == nil {
		return deny(http.StatusForbidden, "no route class for host "+req.Host)
	}
	raw := bearer(req.Authorization)
	if raw == "" && route.AccessTokenCookie != "" {
		raw = req.Cookies[route.AccessTokenCookie]
	}
	if raw == "" {
		return unauthenticated(route, "no token")
	}
	id, err := d.verify.Verify(ctx, raw)
	if err != nil {
		return unauthenticated(route, "token refused: "+err.Error())
	}
	if id.SessionID == "" {
		return unauthenticated(route, "token names no session")
	}
	who := identity{subject: id.Subject, realm: id.Realm, session: id.SessionID, email: id.Email, name: id.Name}
	if cached, ok := d.cache.get(id.Subject, id.SessionID, route.Host); ok {
		return allow(route, cached)
	}
	user, err := authz.User(id.Subject)
	if err != nil {
		return deny(http.StatusUnauthorized, err.Error())
	}
	session, err := authz.Session(id.SessionID)
	if err != nil {
		return deny(http.StatusUnauthorized, err.Error())
	}
	revoked, err := d.store.Check(ctx, req.ID, user, "revoked", session)
	if err != nil {
		d.log.WarnContext(ctx, "store unreachable; failing closed", "host", req.Host, "error", err.Error())
		return deny(http.StatusServiceUnavailable, "authorization store unreachable")
	}
	if revoked {
		return deny(http.StatusForbidden, "session revoked")
	}
	ok, err := d.store.Check(ctx, req.ID, user, route.Relation, route.Object)
	if err != nil {
		d.log.WarnContext(ctx, "store unreachable; failing closed", "host", req.Host, "error", err.Error())
		return deny(http.StatusServiceUnavailable, "authorization store unreachable")
	}
	if !ok {
		return deny(http.StatusForbidden, "not "+route.Relation+" on "+route.Object)
	}
	d.cache.put(id.Subject, id.SessionID, route.Host, who)
	return allow(route, who)
}

func bearer(h string) string {
	const prefix = "bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func deny(status int, reason string) Decision {
	return Decision{Allow: false, Status: status, Reason: reason}
}

// identityHeaders are what a backend may trust, so the shim owns them: set
// on an identified request, stripped on every other.
var identityHeaders = []string{HeaderSubject, HeaderRealm, HeaderSession, HeaderEmail, HeaderName}

// unauthenticated answers a request that carries no valid token. On a bearer
// route that is a refusal. On an oidc route it is not this shim's question:
// Envoy Gateway runs ext_authz before its OIDC filter, so the request goes on
// -- stripped of every identity header and of whatever bearer it carried --
// to the OIDC filter, which sends it to sign in or completes the code flow.
// Nothing reaches a backend on an oidc route without a session that filter
// established, and every request that has one comes back through here.
func unauthenticated(route *Route, reason string) Decision {
	if route.AuthMode != AuthModeOIDC {
		return deny(http.StatusUnauthorized, reason)
	}
	return Decision{
		Allow:         true,
		Identified:    false,
		Reason:        reason,
		RemoveHeaders: append([]string{"authorization"}, identityHeaders...),
	}
}

func allow(route *Route, who identity) Decision {
	dec := Decision{
		Allow:      true,
		Identified: true,
		Headers: map[string]string{
			HeaderSubject: who.subject,
			HeaderRealm:   who.realm,
			HeaderSession: who.session,
			HeaderEmail:   who.email,
			HeaderName:    who.name,
		},
	}
	// The edge token is valid at the director and at every sibling; a
	// backend gets it only where its exposure says forwardToken (AD-13).
	if !route.ForwardToken {
		dec.RemoveHeaders = []string{"authorization"}
	}
	return dec
}
