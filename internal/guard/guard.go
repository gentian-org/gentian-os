/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package guard is the steps every user-callable route of the director, the
// registrar and the usher takes before it does anything: the request's id,
// who is calling, and whether they may do the route's relation to its
// object.
//
// The service still says everything that is its own: the realm its
// challenge names, how it answers a refusal, which relation a route needs
// and what the route's object is. The guard only takes the steps in the
// same order everywhere, so that a refusal in one service is a refusal in
// another for the same reason.
package guard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

type ctxRequestID struct{}

// inboundID is what a request id from the gateway must look like to be kept.
// It ends up in commit trailers, Keycloak's admin events and the decision
// log, so it is matched, not escaped.
var inboundID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// RequestID keeps the gateway's id so the audit logs join on one value
// (security principle 7), and mints one for callers that arrive without.
func RequestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); inboundID.MatchString(id) {
		return id
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Identify gives r its request id: answered in X-Request-Id, and carried in
// the returned request's context for ID.
func Identify(w http.ResponseWriter, r *http.Request) *http.Request {
	id := RequestID(r)
	w.Header().Set("X-Request-Id", id)
	return r.WithContext(context.WithValue(r.Context(), ctxRequestID{}, id))
}

// ID is the request id Identify gave the request ctx belongs to.
func ID(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID{}).(string)
	return id
}

// Bearer is the request's bearer credential, or empty. The scheme is matched
// in any case and the credential trimmed.
func Bearer(r *http.Request) string {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// Authenticator verifies the token a request carries.
type Authenticator interface {
	FromRequest(r *http.Request) (*authn.Identity, error)
}

// Guard takes the steps for one service.
type Guard struct {
	Authn Authenticator
	Authz authz.Checker
	// Realm is named in the challenge a caller who is not known gets:
	// WWW-Authenticate: Bearer realm="<Realm>".
	Realm string
	// Scoped holds a delegated token -- a person's token a component
	// obtained by vouching -- to the routes its scopes name
	// (authn.Identity.MayCall), before the store is asked. Authorize only.
	Scoped bool
	Log    *slog.Logger
	// Fail answers a refusal in the service's own shape.
	Fail func(w http.ResponseWriter, r *http.Request, code int, msg string)
}

// Caller is who passed the guard, and what they were allowed.
type Caller struct {
	Ident *authn.Identity
	// User is the caller as OpenFGA's user.
	User string
	// Holder is the caller as a role holder of their token's realm
	// (authz.HolderUser), asked when User is refused; empty when the realm
	// cannot be named.
	Holder string
	// Relation and Object are what was asked and held; empty from Identify.
	Relation, Object string
}

// Decision is the relation and object that permitted the call, in the form
// a commit trailer carries: "can_view tenant:demo".
func (c Caller) Decision() string { return c.Relation + " " + c.Object }

// Identify knows who is calling and asks nothing of the store: for a caller
// asking about themselves. On refusal it has already answered, and ok is
// false.
func (g *Guard) Identify(w http.ResponseWriter, r *http.Request, route string) (c Caller, ok bool) {
	ident, user, ok := g.authenticate(w, r, route)
	if !ok {
		return Caller{}, false
	}
	holder, _ := authz.HolderUser(ident.Realm, ident.Subject)
	return Caller{Ident: ident, User: user, Holder: holder}, true
}

// Authorize knows who is calling and asks whether they may do a relation to
// the route's object. relations names, once the caller is known, the
// relations any one of which opens the route; they are asked in order, each
// as the account and, refused, as the realm's role holder (authz.CheckAs),
// and the first held answers. A store that does not answer is a refusal. On
// refusal it has already answered, and ok is false.
func (g *Guard) Authorize(
	w http.ResponseWriter, r *http.Request, route string,
	relations func(*authn.Identity) []string, object func(*http.Request) (string, error),
) (c Caller, ok bool) {
	ctx := r.Context()
	ident, user, ok := g.authenticate(w, r, route)
	if !ok {
		return Caller{}, false
	}
	if g.Scoped && !ident.MayCall(route) {
		g.Log.WarnContext(ctx, "delegated token refused outside its scopes", "request_id", ID(ctx),
			"client", ident.Client, "route", route)
		g.Fail(w, r, http.StatusForbidden, "this token was obtained on the person's behalf and does not reach this route")
		return Caller{}, false
	}
	target, err := object(r)
	if err != nil {
		g.Fail(w, r, http.StatusBadRequest, "invalid name")
		return Caller{}, false
	}
	// A cluster role the claim gives an account of a tenant's realm is held
	// by that realm's account and asked as it; a token's realm is the one
	// its signature was checked against.
	holder, _ := authz.HolderUser(ident.Realm, ident.Subject)
	for _, relation := range relations(ident) {
		allowed, err := authz.CheckAs(ctx, g.Authz, ID(ctx), user, holder, relation, target)
		if err != nil {
			// The decision point is unreachable: refuse, and say it is us.
			g.Fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
			return Caller{}, false
		}
		if allowed {
			return Caller{Ident: ident, User: user, Holder: holder, Relation: relation, Object: target}, true
		}
	}
	g.Fail(w, r, http.StatusForbidden, "forbidden")
	return Caller{}, false
}

// authenticate verifies the caller and names them as OpenFGA's user.
func (g *Guard) authenticate(w http.ResponseWriter, r *http.Request, route string) (*authn.Identity, string, bool) {
	ctx := r.Context()
	ident, err := g.Authn.FromRequest(r)
	if err != nil {
		g.Log.WarnContext(ctx, "authentication failed", "request_id", ID(ctx), "reason", err.Error(), "route", route)
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+g.Realm+`"`)
		g.Fail(w, r, http.StatusUnauthorized, "unauthenticated")
		return nil, "", false
	}
	user, err := authz.User(ident.Subject)
	if err != nil {
		g.Log.WarnContext(ctx, "authentication failed", "request_id", ID(ctx), "reason", err.Error(), "route", route)
		g.Fail(w, r, http.StatusUnauthorized, "unauthenticated")
		return nil, "", false
	}
	return ident, user, true
}

// One is relations for a route one relation opens.
func One(relation string) func(*authn.Identity) []string {
	return func(*authn.Identity) []string { return []string{relation} }
}
