/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// Who a component may speak for.
//
// A component whose profile declares requires.services.vouching can obtain a
// person's token from the tenant's realm without that person being there.
// The realm answers it only for a person who is linked to the component's
// entry in the realm, so the link is the whole of the control: whoever can
// make one decides whose tokens the component can have.
//
// So the two directions are not open to the same callers.
//
// MAKING a link gives the component something, and only the person it is
// about can give it. A person is linked on a request that carries that
// person's own token, of the tenant's own realm, and the person linked is
// the token's subject: there is no field to name somebody else in. Not an
// administrator and not the component links anybody.
//
// TAKING a link away gives nobody anything, so three callers may: the person
// themself, whoever manages the tenant's people, and the component, which
// presents the key the operator gave it and can reach the links to itself
// alone. The same three may ask whether a person is linked.

// linkHolders registers a route about one person's link, for the three
// callers who may read it or take it away.
//
// It is the one registration besides guarded, and exists because one of the
// three is not a person. A bearer that is a component's key is recognised
// before anything tries to read it as a token, and is then held to its own
// tenant and its own profile (admitComponent). Every other caller goes the
// way of every other route: a verified token and a question to the store.
// Which question depends on whom they ask about: for their own link it is
// whether they may enter the tenant, and for anybody else's whether they
// manage its people.
//
// A key opens these routes and nothing else. No other route looks for one,
// and a key is not a token, so everywhere else it is a caller nobody could
// identify.
func (s *Server) linkHolders(pattern string, h func(http.ResponseWriter, *http.Request, call)) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if holder := s.cfg.VouchingKeys.holder(bearerOf(r)); holder != nil {
			if c, ok := s.admitComponent(w, r, pattern, holder); ok {
				h(w, r, c)
			}
			return
		}
		relation := func(ident *authn.Identity) string {
			if ident.Subject == r.PathValue("id") {
				return "can_enter"
			}
			return "can_manage_users"
		}
		if c, ok := s.authorizeAs(w, r, pattern, relation, tenantObject); ok {
			h(w, s.holdApprovers(r, c), c)
		}
	})
}

// admitComponent holds a component's key to what it is for: the tenant the
// component runs in and the profile it runs, both as the operator wrote them
// beside the key's hash. A key of another tenant, or of another component of
// this one, is a known caller asking for what is not theirs, and is told so.
func (s *Server) admitComponent(w http.ResponseWriter, r *http.Request, pattern string, holder *VouchingKey) (call, bool) {
	ctx := r.Context()
	tenant, profile := r.PathValue("t"), r.PathValue("profile")
	if !dnsLabel.MatchString(tenant) || !dnsLabel.MatchString(profile) {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return call{}, false
	}
	if holder.Tenant != tenant || holder.Component != profile {
		s.cfg.Log.WarnContext(ctx, "refused: a component's key was used outside its own tenant and profile",
			"request_id", reqID(ctx), "route", pattern, "key_tenant", holder.Tenant, "key_component", holder.Component,
			"tenant", tenant, "profile", profile)
		s.fail(w, r, http.StatusForbidden,
			"forbidden: a component's key reads and removes the links to that component, in its own tenant, and no others")
		return call{}, false
	}
	return call{
		component: "component:" + holder.Tenant + "/" + holder.Component,
		decision:  "own_key vouching:" + holder.Tenant + "/" + holder.Component,
	}, true
}

// bearerOf is the request's bearer credential, or empty.
func bearerOf(r *http.Request) string {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// vouchingProfile reads the profile in the request path. On failure it has
// already answered, and ok is false.
func (s *Server) vouchingProfile(w http.ResponseWriter, r *http.Request) (string, bool) {
	profile := r.PathValue("profile")
	if !dnsLabel.MatchString(profile) {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return "", false
	}
	return profile, true
}

// ownToken refuses a caller who names themself with a token another realm
// issued. A subject is an id within one realm, and the same id under another
// realm's signature is not the same person. On refusal it has already
// answered, and ok is false.
func (s *Server) ownToken(w http.ResponseWriter, r *http.Request, c call, realm identity.Realm) bool {
	if c.realm == realm.Name() {
		return true
	}
	s.cfg.Log.WarnContext(r.Context(), "refused: a link is a person's own only with a token of the tenant's realm",
		"request_id", reqID(r.Context()), "tenant", r.PathValue("t"), "realm", realm.Name(), "token_realm", c.realm)
	s.fail(w, r, http.StatusForbidden,
		"forbidden: this token was issued by the realm "+c.realm+", and this tenant's people are in "+realm.Name()+
			". Sign in to this tenant and ask again.")
	return false
}

// linkVouching links the caller to a component that vouches for people.
//
// The route asks can_enter, which admits everybody who may be in the tenant
// at all; that it is the caller who is linked is not something a relation
// can say, and is said here: the person is the token's subject and nothing
// in the request can name another.
func (s *Server) linkVouching(w http.ResponseWriter, r *http.Request, c call) {
	profile, ok := s.vouchingProfile(w, r)
	if !ok {
		return
	}
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	if !s.ownToken(w, r, c, realm) {
		return
	}
	err := s.cfg.Identity.LinkVouching(identityContext(r), realm, c.subject, profile)
	switch {
	case errors.Is(err, identity.ErrNoSuchIssuer):
		s.fail(w, r, http.StatusNotFound, "no component named "+profile+" vouches for people in this tenant: "+
			"its profile has to declare requires.services.vouching and be installed here")
		return
	case err != nil:
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "link-vouching:"+profile, realm, c.subject)
	w.WriteHeader(http.StatusNoContent)
}

// vouchingLinked answers whether a person is linked to a component.
func (s *Server) vouchingLinked(w http.ResponseWriter, r *http.Request, c call) {
	profile, ok := s.vouchingProfile(w, r)
	if !ok {
		return
	}
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if c.subject == id && !s.ownToken(w, r, c, realm) {
		return
	}
	linked, err := s.cfg.Identity.VouchingLinked(identityContext(r), realm, id, profile)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "profile": profile, "person": id, "linked": linked})
}

// unlinkVouching takes a person's link to a component away. Somebody who is
// not linked is left as they are, and the answer is the same.
func (s *Server) unlinkVouching(w http.ResponseWriter, r *http.Request, c call) {
	profile, ok := s.vouchingProfile(w, r)
	if !ok {
		return
	}
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if c.subject == id && !s.ownToken(w, r, c, realm) {
		return
	}
	if err := s.cfg.Identity.UnlinkVouching(identityContext(r), realm, id, profile); err != nil {
		s.identityError(w, r, err)
		return
	}
	s.recordIdentityAction(r, c, "unlink-vouching:"+profile, realm, id)
	w.WriteHeader(http.StatusNoContent)
}
