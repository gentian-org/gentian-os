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
	"net/url"
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
	// Browser is true when a refusal will be read by a person rather than by
	// a program: an oidc route, where the caller followed a link. It decides
	// whether the denial carries a page or the bare status.
	Browser bool
	// Redirect makes this decision a 302 to that location instead of a
	// refusal with a body. Only sign-out uses it: the request is answered
	// here and never reaches a backend, which is the point.
	Redirect string
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
// edgeOAuth2Prefix is where Envoy Gateway's OIDC filter answers: the
// callback that completes a sign-in and the path that ends a session.
const edgeOAuth2Prefix = "/oauth2/"

// SignOutPath is the one path this service answers itself.
//
// Signing out of the edge is not signing out. Envoy Gateway's own logout path
// clears the zone's cookies and sends the browser to the realm, but with no
// id_token_hint, and Keycloak will not end a session it cannot attribute
// without asking the person to confirm. That confirmation page is the "another
// screen" a person sees between pressing sign out and arriving back where they
// started, and it exists for a good reason: a logout request that names no
// session might have come from a link on somebody else's site.
//
// The hint is in the browser already, in the zone's own ID token cookie. So
// this path reads it, answers a redirect that carries it, and Keycloak ends
// the session without asking. The post-logout target is the zone's own
// /oauth2/logout, so the last thing that happens is Envoy dropping its
// cookies: end the realm session first, then the edge's, because the reverse
// order throws away the hint before it has been used.
// Under /oauth2/ because that prefix is routed on every host in a zone: the
// Keycloak console route carries it explicitly and every component exposure
// gets it, so a path anywhere else would be a 404 from Envoy before this
// service ever saw it. Envoy's own OAuth2 filter claims only its callback and
// its logout path and passes the rest through, and ext_authz runs ahead of it
// in any case, so this one is answered here.
const SignOutPath = "/oauth2/sign-out"

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
	// The edge's own endpoints are not the app's, and this service must not
	// have an opinion about them. /oauth2/callback finishes a sign-in that by
	// definition has no session yet, and /oauth2/logout ends one that may
	// already be refused here -- which is how signing out broke: Keycloak's
	// back-channel logout recorded the session as revoked, so the redirect
	// back to /oauth2/logout was answered 403 by this service and the Gateway
	// never got to drop its cookies. A revoked session must still be able to
	// reach the path that clears it.
	if req.Path == SignOutPath {
		return signOut(route, req)
	}
	if strings.HasPrefix(req.Path, edgeOAuth2Prefix) {
		return Decision{Allow: true, RemoveHeaders: append([]string{"authorization"}, identityHeaders...)}
	}
	// Where the SESSION is, which is what the route's auth mode says and
	// nothing else.
	//
	// This used to read the Authorization header first and fall back to the
	// cookie, and that is wrong on an oidc route in a way that took a long
	// time to see. On such a route the session is the zone's cookie; a bearer
	// in the header belongs to the BACKEND and is none of this service's
	// business. Reading it as the session means any page that calls its own
	// API with its own token has that token judged against the zone's
	// audience, fails, and is treated as having no session at all -- which
	// then strips the header, so the backend is asked to authenticate a
	// request carrying nothing.
	//
	// That is exactly what broke Keycloak's administration console. The page
	// holds a token minted for realm-management, the edge expects one minted
	// for the director, and the console's own Admin REST call arrived at
	// Keycloak stripped bare and was answered 401.
	raw := ""
	if route.AuthMode == AuthModeOIDC && route.AccessTokenCookie != "" {
		raw = req.Cookies[route.AccessTokenCookie]
	}
	if raw == "" {
		raw = bearer(req.Authorization)
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
	// No revocation question.
	//
	// This used to ask whether the session had been recorded as revoked, a
	// tuple the director wrote on back-channel logout. That whole path is
	// gone. Ending the session at Keycloak is what ends it: the edge holds a
	// short-lived access token and refreshes it, and a refresh against an
	// ended session fails. The bound is the access token's lifetime, which
	// the realm sets, and this costs one fewer round trip per request than
	// asking did.
	ok, err := d.store.Check(ctx, req.ID, user, route.Relation, route.Object)
	if err != nil {
		d.log.WarnContext(ctx, "store unreachable; failing closed", "host", req.Host, "error", err.Error())
		return deny(http.StatusServiceUnavailable, "authorization store unreachable")
	}
	if !ok {
		return browserRefusal(route, deny(http.StatusForbidden, "not "+route.Relation+" on "+route.Object))
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
	// Identity headers always go: a request with no session must not arrive
	// carrying any, or a backend that trusts them trusts a forgery.
	//
	// The Authorization header is a different question. Normally it goes too,
	// because a backend behind the zone has no use for the edge's token and
	// should not be handed one. But on a route that keeps the caller's own
	// bearer, the header is the backend's business and removing it turns a
	// request the backend could have authenticated into one it cannot.
	remove := append([]string(nil), identityHeaders...)
	if !route.KeepClientToken {
		remove = append([]string{"authorization"}, identityHeaders...)
	}
	return Decision{
		Allow:         true,
		Identified:    false,
		Reason:        reason,
		RemoveHeaders: remove,
	}
}

// browserRefusal marks a denial that a person will see, so it can be answered
// with something they can act on.
func browserRefusal(route *Route, dec Decision) Decision {
	if route != nil && route.AuthMode == AuthModeOIDC {
		dec.Browser = true
	}
	return dec
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
	// A route that keeps the caller's own token is not stripped either: its
	// backend authenticates the bearer the page already holds.
	if !route.ForwardToken && !route.KeepClientToken {
		dec.RemoveHeaders = []string{"authorization"}
	}
	return dec
}

// signOut answers the sign-out path with a redirect the person never sees.
//
// It asks the authorization store nothing. Ending your own session is not a
// permission: a session that is refused everywhere must still be able to end
// itself, which is the same reason the edge's own /oauth2/ paths pass through
// untouched. Requiring a relation here would mean the one person who most
// needs to sign out, someone whose account was just deleted, could not.
func signOut(route *Route, req Request) Decision {
	// Where the browser ends up either way: Envoy's logout path, which drops
	// the zone's cookies and returns to the portal.
	local := "https://" + hostOnly(req.Host) + edgeOAuth2Prefix + "logout"
	hint := ""
	if route.IDTokenCookie != "" {
		hint = req.Cookies[route.IDTokenCookie]
	}
	if route.EndSessionURL == "" || hint == "" {
		// No hint to offer, so nothing is gained by going to the realm first.
		// Clearing the edge's cookies still signs the person out of every
		// kernel host; the realm session outlives it until its own idle
		// timeout, which is the behaviour this had before.
		return Decision{Status: http.StatusFound, Redirect: local, Reason: "sign out, edge only"}
	}
	target := route.EndSessionURL + "?id_token_hint=" + url.QueryEscape(hint) +
		"&post_logout_redirect_uri=" + url.QueryEscape(local)
	return Decision{Status: http.StatusFound, Redirect: target, Reason: "sign out"}
}

// hostOnly drops the port. A Host header carries one on a non-default port,
// and a post_logout_redirect_uri that carries it will not match what the
// client registered.
func hostOnly(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}
