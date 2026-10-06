/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// router is what the route registrars take, so the guard cannot be forgotten
// by registering against the bare mux: the only implementation here wraps
// every handler.
type router interface {
	HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request))
}

// guardedMux registers every handler behind the token check.
type guardedMux struct {
	mux  *http.ServeMux
	auth func(http.HandlerFunc) http.HandlerFunc
}

func (g *guardedMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	g.mux.HandleFunc(pattern, g.auth(h))
}

// HTTPServer exposes app lifecycle operations over HTTP.
type HTTPServer struct {
	Service *Service
	Addr    string
	// Token is the shared secret the director presents. Every request except
	// /healthz must carry it as a bearer.
	//
	// This API used to authenticate nobody: it is reachable by anything that
	// can reach the Service, it takes and deletes backups and purges an app's
	// data, and each took the actor's name from an X-Gentian-Actor header,
	// which is a claim and not a proof. Any pod in any namespace could take a
	// backup and have somebody else's name recorded against it.
	//
	// A shared token rather than the caller's own: verifying that would mean
	// the operator holding the issuer's keys and the authorization graph,
	// which is the director's job and not this process's. The token is what
	// makes the actor header worth the paper it is written on, because only
	// the director can set it.
	//
	// Empty refuses everything rather than admitting everything. A
	// misconfigured deployment that drops the secret must not quietly become
	// the open API this replaces.
	Token string
}

// authenticated wraps a handler so it is reached only by a caller presenting
// the shared token. Constant-time compared: the token is a fixed string and
// an early-exit comparison leaks its prefix to anything that can time it.
func (h *HTTPServer) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.Token == "" {
			writeErr(w, http.StatusServiceUnavailable, errors.New(
				"this API has no token configured and refuses every request; set appLifecycle.tokenSecretRef"))
			return
		}
		presented := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if subtle.ConstantTimeCompare([]byte(presented), []byte(h.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("this API is the director's; present its token"))
			return
		}
		next(w, r)
	}
}

// routes is everything this listener serves.
//
// Installing an app, removing one and choosing its addons are not here. They
// are changes to what a tenant is meant to have, the director makes them as
// commits, and the operator learns of them from git like everything else. The
// routes that once did them here, and the checkout and push credential they
// needed, are gone.
func (h *HTTPServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Everything below /v1 is the director's, reads included: a tenant's
	// installed apps, its usage and its backups are its own business.
	guarded := &guardedMux{mux: mux, auth: h.authenticated}
	guarded.HandleFunc("GET /v1/tenants/{tenant}/apps", h.handleList)
	h.registerAppRoutes(guarded)
	h.registerResourceRoutes(guarded)
	h.registerBackupRoutes(guarded)
	h.registerPlatformRoutes(guarded)
	h.registerNotificationRoutes(guarded)
	h.registerTenantRoutes(guarded)
	h.registerImportRoutes(guarded)
	return mux
}

// Start runs the HTTP server until ctx is cancelled.
func (h *HTTPServer) Start(ctx context.Context) error {
	if h.Service == nil {
		return errors.New("applifecycle service is nil")
	}
	mux := h.routes()

	srv := &http.Server{Addr: h.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func (h *HTTPServer) handleList(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	apps, err := h.Service.ListInstalled(r.Context(), tenant)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": tenant, "apps": apps})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"detail": strings.TrimSpace(err.Error())})
}
