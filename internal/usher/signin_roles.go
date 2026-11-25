/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usher

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/membership"
)

// SignInRoles reads which roles a person holds on an app and on the add-ons
// activated in it.
type SignInRoles interface {
	SignInRoles(ctx context.Context, requestID, tenant, profile, sub string) ([]authz.AppRoleSet, error)
}

// SignInRolesConfig is what the sign-in question is answered from.
type SignInRolesConfig struct {
	// Keys are the public keys of the identity provider's listener, by id:
	// the question is answered to a request one of them signed, and to no
	// other.
	Keys  map[string]ed25519.PublicKey
	Roles SignInRoles
	// Checker asks the store about the cluster's and a tenant's own roles;
	// Cluster is the cluster's id. Without either the cluster question is
	// refused.
	Checker authz.Checker
	Cluster string
	Log     *slog.Logger
	// Now is the clock a signature's age is measured by; time.Now when nil.
	Now func() time.Time
}

type signInRolesRequest struct {
	// Tenant and App name the app the person signs in to, as the platform
	// wrote them on the app's sign-in client. Tenant without App asks for
	// the person's roles on the tenant itself.
	Tenant string `json:"tenant,omitempty"`
	App    string `json:"app,omitempty"`
	// Cluster asks for the cluster roles the person holds, as the account
	// of Realm: the roles a cluster's own tools read from a token.
	Cluster bool   `json:"cluster,omitempty"`
	Realm   string `json:"realm,omitempty"`
	// Person is the subject of the person signing in.
	Person string `json:"person"`
}

type signInRolesResponse struct {
	// Apps is always there, empty when no app was asked about: an answer
	// without it is not one.
	Apps []authz.AppRoleSet `json:"apps"`
	// Tenant are the roles held on the tenant itself: admin, user.
	Tenant []string `json:"tenant,omitempty"`
	// Cluster are the cluster roles held, by the model's names.
	Cluster []string `json:"cluster,omitempty"`
}

// held is the relations of a set the person holds on one object, sorted.
func held(ctx context.Context, c authz.Checker, requestID, user, holder, object string, relations []string) ([]string, error) {
	answers, err := authz.Holds(ctx, c, requestID, user, holder, object, relations)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, rel := range relations {
		if answers[rel] {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, nil
}

// maxSignInRolesBytes caps the question: three names.
const maxSignInRolesBytes = 4096

// SignInRolesHandler answers the identity provider's question at a sign-in:
// which roles does this person hold on this app. It is served on a listener
// of its own, apart from the one consoles reach, so that the network admits
// the identity provider to it and to nothing else of this service.
//
// The asker is known by its signature, the one it states memberships with.
// A person's token is not asked for: at a sign-in there is none yet.
func SignInRolesHandler(cfg SignInRolesConfig) (http.Handler, error) {
	if len(cfg.Keys) == 0 || cfg.Roles == nil {
		return nil, errors.New("usher: the sign-in question needs a listener key and the store")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sign-in-roles", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSignInRolesBytes+1))
		if err != nil || len(body) > maxSignInRolesBytes {
			http.Error(w, "question too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err := membership.VerifySignature(cfg.Keys, r.Header.Get(membership.SignatureHeader), body, cfg.Now()); err != nil {
			cfg.Log.WarnContext(ctx, "sign-in question refused", "reason", err.Error())
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		var req signInRolesRequest
		if err := json.Unmarshal(body, &req); err != nil || req.Person == "" ||
			(req.Tenant == "" && !req.Cluster) || (req.Tenant == "" && req.App != "") ||
			(req.Tenant != "" && !authz.ValidName(req.Tenant)) || (req.App != "" && !authz.ValidName(req.App)) {
			http.Error(w, "the body must be JSON with person, and tenant (with or without app) or cluster", http.StatusBadRequest)
			return
		}
		user, err := authz.User(req.Person)
		if err != nil {
			http.Error(w, "person must be a subject", http.StatusBadRequest)
			return
		}
		requestID := r.Header.Get("x-request-id")
		unanswered := func(err error) {
			// Unanswered is not "no roles": the asker fails the sign-in.
			cfg.Log.WarnContext(ctx, "store unreachable; the sign-in question is not answered",
				"tenant", req.Tenant, "app", req.App, "cluster", req.Cluster, "error", err.Error())
			http.Error(w, "authorization store unreachable", http.StatusServiceUnavailable)
		}
		answer := signInRolesResponse{Apps: []authz.AppRoleSet{}}
		switch {
		case req.App != "":
			answer.Apps, err = cfg.Roles.SignInRoles(ctx, requestID, req.Tenant, req.App, req.Person)
		case req.Tenant != "":
			if cfg.Checker == nil {
				http.Error(w, "a tenant's roles are not answered here", http.StatusNotFound)
				return
			}
			answer.Tenant, err = held(ctx, cfg.Checker, requestID, user, "", authz.Tenant(req.Tenant), []string{authz.RoleAdmin, authz.RoleUser})
		}
		if err != nil {
			unanswered(err)
			return
		}
		if req.Cluster {
			if cfg.Checker == nil || cfg.Cluster == "" {
				http.Error(w, "the cluster's roles are not answered here", http.StatusNotFound)
				return
			}
			holder, err := authz.HolderUser(req.Realm, req.Person)
			if err != nil {
				http.Error(w, "cluster roles are asked with the realm of the account", http.StatusBadRequest)
				return
			}
			answer.Cluster, err = held(ctx, cfg.Checker, requestID, user, holder, authz.Cluster(cfg.Cluster), authz.ClusterRoles)
			if err != nil {
				unanswered(err)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	})
	return mux, nil
}
