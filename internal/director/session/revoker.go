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

// Package session records the sessions the issuer has ended.
//
// Envoy Gateway's OIDC filter has no back-channel endpoint and the edge
// session is a signed cookie in the browser, so there is no server-side
// session for a logout token to end. The shim is where revocation is
// enforced, but it runs as several replicas and keeps no state; so every
// zone client's back-channel logout URI points here, and what arrives is
// written to the store, where every shim replica reads it (networking.md §4).
package session

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
)

// Store is the part of the authorization store revocation needs.
type Store interface {
	RevokeSession(ctx context.Context, sub, sid string) error
	SweepRevocations(ctx context.Context, olderThan time.Time) (int, error)
}

// Verifier checks a logout token.
type Verifier interface {
	VerifyLogout(ctx context.Context, raw string) (*authn.Logout, error)
}

// Revoker is the back-channel logout endpoint and the sweep that retires
// what it wrote.
type Revoker struct {
	verifier Verifier
	store    Store
	// TTL is how long a revocation is kept: the longest a token of the ended
	// session could still be presented, after which there is nothing to deny.
	ttl time.Duration
	log *slog.Logger
	now func() time.Time
}

// New returns a Revoker. ttl bounds how long a revocation is kept.
func New(v Verifier, store Store, ttl time.Duration, log *slog.Logger) (*Revoker, error) {
	if v == nil || store == nil {
		return nil, errors.New("session: a verifier and a store are required")
	}
	if ttl <= 0 {
		return nil, errors.New("session: the revocation TTL must be positive")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Revoker{verifier: v, store: store, ttl: ttl, log: log, now: time.Now}, nil
}

const maxLogoutTokenBytes = 16 << 10

// ServeHTTP is the OpenID Connect back-channel logout endpoint (§2.5): a form
// with logout_token, answered 200 once the session is recorded as ended and
// 400 for anything that is not a logout token of this platform's issuer.
// Keycloak retries on failure, so a store that is briefly unreachable is a
// 503, not a lost logout.
func (rc *Revoker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxLogoutTokenBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	raw := r.PostForm.Get("logout_token")
	if raw == "" {
		http.Error(w, "logout_token is required", http.StatusBadRequest)
		return
	}
	logout, err := rc.verifier.VerifyLogout(ctx, raw)
	if err != nil {
		rc.log.WarnContext(ctx, "logout token refused", "reason", err.Error())
		http.Error(w, "invalid logout token", http.StatusBadRequest)
		return
	}
	if err := rc.store.RevokeSession(ctx, logout.Subject, logout.SessionID); err != nil {
		rc.log.ErrorContext(ctx, "session not revoked", "realm", logout.Realm, "session", logout.SessionID, "error", err.Error())
		http.Error(w, "revocation not recorded", http.StatusServiceUnavailable)
		return
	}
	rc.log.InfoContext(ctx, "session revoked", "realm", logout.Realm, "user", logout.Subject, "session", logout.SessionID)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// Run sweeps expired revocations until ctx ends: once at start, for what an
// earlier process wrote, then every interval.
func (rc *Revoker) Run(ctx context.Context, interval time.Duration) {
	rc.sweep(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rc.sweep(ctx)
		}
	}
}

func (rc *Revoker) sweep(ctx context.Context) {
	n, err := rc.store.SweepRevocations(ctx, rc.now().Add(-rc.ttl))
	if err != nil {
		rc.log.WarnContext(ctx, "revocations not swept", "error", err.Error())
		return
	}
	if n > 0 {
		rc.log.InfoContext(ctx, "revocations swept", "retired", n)
	}
}
