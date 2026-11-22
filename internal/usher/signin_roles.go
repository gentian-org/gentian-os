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
	Log   *slog.Logger
	// Now is the clock a signature's age is measured by; time.Now when nil.
	Now func() time.Time
}

type signInRolesRequest struct {
	// Tenant and App name the app the person signs in to, as the platform
	// wrote them on the app's sign-in client.
	Tenant string `json:"tenant"`
	App    string `json:"app"`
	// Person is the subject of the person signing in.
	Person string `json:"person"`
}

type signInRolesResponse struct {
	Apps []authz.AppRoleSet `json:"apps"`
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
			!authz.ValidName(req.Tenant) || !authz.ValidName(req.App) {
			http.Error(w, "the body must be JSON with tenant, app and person", http.StatusBadRequest)
			return
		}
		apps, err := cfg.Roles.SignInRoles(ctx, r.Header.Get("x-request-id"), req.Tenant, req.App, req.Person)
		if err != nil {
			if errors.Is(err, authz.ErrInvalidID) {
				http.Error(w, "person must be a subject", http.StatusBadRequest)
				return
			}
			// Unanswered is not "no roles": the asker fails the sign-in.
			cfg.Log.WarnContext(ctx, "store unreachable; the sign-in question is not answered",
				"tenant", req.Tenant, "app", req.App, "error", err.Error())
			http.Error(w, "authorization store unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(signInRolesResponse{Apps: apps})
	})
	return mux, nil
}
