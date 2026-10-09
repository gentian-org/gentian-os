/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// Verifier checks a token against the platform's issuer.
type Verifier interface {
	// Verify checks an access token.
	Verify(ctx context.Context, raw string) (*authn.Identity, error)
	// VerifyIDToken checks an ID token issued to the named client.
	VerifyIDToken(ctx context.Context, raw, client string) (*authn.Identity, error)
}

// Store is the part of the authorization store L2 needs: a decision, and the
// changelog that says when a decision may have changed.
type Store interface {
	Check(ctx context.Context, requestID, user, relation, object string) (bool, error)
	Changes(ctx context.Context, objectType, token string) ([]authz.Change, string, error)
}

// Request is what the Gateway tells the bouncer about a request.
//
// The session's cookies are the OAuth2 filter's: it encrypts them, and it
// alone decides whether they are a session. Who is asking is never taken
// from Cookie; what this service is given to decide on is what that filter
// made of them. The header is here for one purpose, which is to pass it on
// without the session's cookies in it (Route.SessionCookies).
type Request struct {
	ID            string
	Host          string
	Path          string
	Authorization string
	// IDToken is the value of HeaderIDToken.
	IDToken string
	// Cookie is the Cookie header as it arrived, never read for identity.
	Cookie string
}

// Decision is the bouncer's answer.
type Decision struct {
	Allow bool
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
	// refusal with a body. Only the old sign-out path uses it: the request is
	// answered here and never reaches a backend.
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

	exchange *exchangeCache

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
	// Exchanger obtains the app-bound token of a route that asks for one.
	// Without it such a route is refused: there is nothing to hand its
	// backend in place of the session's token.
	Exchanger Exchanger
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
	return &Decider{
		verify: o.Verifier, store: o.Store, cache: newCache(o.CacheTTL, o.Now), log: o.Logger, now: o.Now, table: o.Table,
		exchange: newExchangeCache(o.Exchanger, o.Now),
	}
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

// LogoutPath is where the Gateway's OAuth2 filter ends a session: it drops
// the host's cookies and sends the browser to the realm's end-session
// endpoint with the session's ID token as the hint, so the realm ends its
// session too without asking, and returns to the host's front page. The
// filter answers it itself; a request to it never arrives here.
const LogoutPath = "/oauth2/logout"

// SignOutPath is the older name for signing out, kept as a redirect.
//
// It was answered here in full while the Gateway's own logout cleared the
// edge's cookies and nothing else: this service read the ID token from its
// cookie and sent the browser to the realm with it. The Gateway does all of
// that now, and the cookie is encrypted, so the path is only an alias. The
// consoles still navigate to it, and a link that worked yesterday should not
// be a 404 from a backend today.
const SignOutPath = "/oauth2/sign-out"

// Identity headers the backend receives. The gateway strips whatever the
// client sent under these names by overriding them, so the app may trust
// one at all.
const (
	HeaderSubject = "x-gentian-subject"
	HeaderRealm   = "x-gentian-realm"
	HeaderSession = "x-gentian-session"
	HeaderEmail   = "x-gentian-email"
	HeaderName    = "x-gentian-name"
)

// HeaderIDToken is where the Gateway's OAuth2 filter puts the session's ID
// token on a route that keeps the caller's own Authorization header.
//
// The filter owns the header: it removes whatever the client sent under this
// name before it does anything else, and sets it only from a session it has
// just validated or just refreshed. It is for this service alone and is
// removed from every request that goes on to a backend.
const HeaderIDToken = "x-gentian-id-token"

// IdentityHeaders are every header of the front door's own: the five a
// backend is told the caller by, and the one the session's ID token travels
// to this service in.
//
// One list, because a route that asks this service nothing has nobody to
// overwrite them: whatever stands on such a route -- the publishing proxy of
// a tenant's DMZ, the sign-in route that takes no session -- removes exactly
// these, and reads them from here. A header added to the block above and not
// to this list fails a test (TestIdentityHeadersAreAllOfThem).
func IdentityHeaders() []string {
	return []string{HeaderSubject, HeaderRealm, HeaderSession, HeaderEmail, HeaderName, HeaderIDToken}
}

// Decide answers whether the request may reach its route.
//
// Fail closed, cached allows carry: a store that cannot be reached leaves
// decisions already cached valid until they expire and answers 503 to
// everything else. Nothing not previously allowed gets through, and nothing
// gets through without a token this service verified itself.
func (d *Decider) Decide(ctx context.Context, req Request) Decision {
	route := d.Table().Match(req.Host)
	if route == nil {
		return deny(http.StatusForbidden, "no route class for host "+req.Host)
	}
	// The old sign-out path, sent on to the Gateway's. Before anything is
	// asked about the caller: ending your own session is not a permission,
	// and a session that is refused everywhere must still be able to end
	// itself. The redirect grants nothing and reaches no backend.
	if route.AuthMode == AuthModeOIDC && pathOnly(req.Path) == SignOutPath {
		return Decision{Status: http.StatusFound, Redirect: LogoutPath, Reason: "sign out"}
	}
	// Deny wins, and it wins before identity is looked at: the profile said
	// this path is not published, so who is asking does not enter into it.
	if route.Denies(req.Path) {
		return deny(http.StatusForbidden, "path is not published on "+req.Host)
	}
	id, err := d.session(ctx, route, req)
	if err != nil {
		// On an oidc route this is not "no session yet". The OAuth2 filter
		// runs first and would have sent such a request to sign in; one that
		// arrives here without a token it set is a request the filter did
		// not vouch for, and the only safe answer is no.
		return browserRefusal(route, deny(http.StatusUnauthorized, err.Error()))
	}
	if id.SessionID == "" {
		return browserRefusal(route, deny(http.StatusUnauthorized, "token names no session"))
	}
	who := identity{subject: id.Subject, realm: id.Realm, session: id.SessionID, email: id.Email, name: id.Name}
	if cached, ok := d.cache.get(id.Subject, id.SessionID, route.Host); ok {
		return d.finish(ctx, route, req, cached)
	}
	user, err := authz.User(id.Subject)
	if err != nil {
		return deny(http.StatusUnauthorized, err.Error())
	}
	// No revocation question. Ending the session at the realm is what ends
	// it: the edge holds a short-lived access token and refreshes it, and a
	// refresh against an ended session fails. The bound is the access token's
	// lifetime, which the realm sets.
	ok, err := d.store.Check(ctx, req.ID, user, route.Relation, route.Object)
	if err != nil {
		d.log.WarnContext(ctx, "store unreachable; failing closed", "host", req.Host, "error", err.Error())
		return deny(http.StatusServiceUnavailable, "authorization store unreachable")
	}
	if !ok {
		return browserRefusal(route, deny(http.StatusForbidden, "not "+route.Relation+" on "+route.Object))
	}
	d.cache.put(id.Subject, id.SessionID, route.Host, who)
	return d.finish(ctx, route, req, who)
}

// finish lets an allowed request through, and on a route that asks for it
// puts the app-bound token in place of the session's.
//
// Fail closed here too. A backend that verifies a token and is handed none
// would refuse the request anyway, with less to say about why; and handing
// it the session's token instead is exactly what the route asked not to get.
func (d *Decider) finish(ctx context.Context, route *Route, req Request, who identity) Decision {
	dec := allow(route, req, who)
	if route.ExchangeScope == "" {
		return dec
	}
	token, err := d.exchange.token(ctx, who, bearer(req.Authorization), route.ExchangeScope)
	if err != nil {
		d.log.WarnContext(ctx, "token exchange failed; failing closed", "host", req.Host, "realm", who.realm, "error", err.Error())
		return deny(http.StatusServiceUnavailable, "the app's token could not be obtained")
	}
	dec.Headers["authorization"] = "Bearer " + token
	kept := dec.RemoveHeaders[:0]
	for _, h := range dec.RemoveHeaders {
		if h != "authorization" {
			kept = append(kept, h)
		}
	}
	dec.RemoveHeaders = kept
	return dec
}

// session verifies the one token the route's mode says proves who is asking.
//
// Exactly one place per mode, and never a fallback from one to another: a
// request that lacks the token its route calls for is refused, not searched
// for something else that might do.
//
//   - bearer: the caller's Authorization header, an access token.
//   - oidc: the Authorization header, which on these routes the OAuth2
//     filter cleared and then set from the session it validated. A bearer the
//     client sent never reaches this service there.
//   - oidc with KeepClientToken: the Authorization header is the page's own
//     and is not read. The filter hands over the session's ID token in
//     HeaderIDToken instead, having removed any the client sent.
func (d *Decider) session(ctx context.Context, route *Route, req Request) (*authn.Identity, error) {
	if route.AuthMode == AuthModeOIDC && route.KeepClientToken {
		if route.IDTokenAudience == "" {
			return nil, errors.New("route names no client to hold its session against")
		}
		if req.IDToken == "" {
			return nil, errors.New("no session token")
		}
		id, err := d.verify.VerifyIDToken(ctx, req.IDToken, route.IDTokenAudience)
		if err != nil {
			return nil, fmt.Errorf("session token refused: %w", err)
		}
		return id, nil
	}
	raw := bearer(req.Authorization)
	if raw == "" {
		return nil, errors.New("no token")
	}
	id, err := d.verify.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("token refused: %w", err)
	}
	return id, nil
}

func bearer(h string) string {
	const prefix = "bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// pathOnly drops the query and the fragment.
func pathOnly(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		return path[:i]
	}
	return path
}

func deny(status int, reason string) Decision {
	return Decision{Allow: false, Status: status, Reason: reason}
}

// browserRefusal marks a denial that a person will see, so it can be answered
// with something they can act on.
func browserRefusal(route *Route, dec Decision) Decision {
	if route != nil && route.AuthMode == AuthModeOIDC {
		dec.Browser = true
	}
	return dec
}

// allow is the only way a request reaches a backend, and it is only reached
// with a verified identity: the identity headers are set on every allowed
// request, replacing whatever the client sent under those names.
//
// What the backend is left with of the session is those headers. The token
// goes out of the Authorization header unless the route forwards it, and the
// session's cookies go out of the Cookie header on every route that has a
// session, forwarded token or not.
func allow(route *Route, req Request, who identity) Decision {
	dec := Decision{
		Allow: true,
		Headers: map[string]string{
			HeaderSubject: who.subject,
			HeaderRealm:   who.realm,
			HeaderSession: who.session,
			HeaderEmail:   who.email,
			HeaderName:    who.name,
		},
		// The ID token header is this service's input and nobody's output.
		// Removed on every route, so one a client sent where the gateway
		// does not own the header goes no further either.
		RemoveHeaders: []string{HeaderIDToken},
	}
	// The edge token is valid at the director and at every sibling; a
	// backend gets it only where its exposure says forwardToken. A route
	// that keeps the caller's own token is not stripped either: its backend
	// authenticates the bearer the page already holds.
	if !route.ForwardToken && !route.KeepClientToken {
		dec.RemoveHeaders = append(dec.RemoveHeaders, "authorization")
	}
	// The Gateway's filter decrypted the session's token cookies into this
	// request, and the backend has no use for them: it is told who is asking
	// in the headers above. The only way to take one cookie out of a request
	// from here is to say what the whole header is to be, so the header is
	// rewritten to the cookies that are not the edge's, as they were, or
	// removed when none is left. A header with none of the edge's cookies in
	// it is not mentioned at all.
	if route.AuthMode == AuthModeOIDC {
		if rest, changed := route.withoutSessionCookies(req.Cookie); changed {
			if strings.Trim(rest, "; \t") == "" {
				dec.RemoveHeaders = append(dec.RemoveHeaders, HeaderCookie)
			} else {
				dec.Headers[HeaderCookie] = rest
			}
		}
	}
	return dec
}
