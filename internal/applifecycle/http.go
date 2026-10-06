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
	"sync/atomic"
	"time"
)

// router is what the route registrars take, so the guard cannot be forgotten
// by registering against the bare mux: the only implementation here wraps
// every handler.
//
// It has two ways to register and a route says which it is. HandleFunc is the
// director's alone: every command, and the reads only the director's own
// commands need. Read is a read of state the usher answers a person with; the
// director's token admits it and so does the reader's.
type router interface {
	HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request))
	Read(pattern string, h func(http.ResponseWriter, *http.Request))
}

// guardedMux registers every handler behind the token check.
type guardedMux struct {
	mux *http.ServeMux
	// auth wraps a handler; reader says whether the reader's token admits it.
	auth func(next http.HandlerFunc, reader bool) http.HandlerFunc
}

func (g *guardedMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	g.mux.HandleFunc(pattern, g.auth(h, false))
}

// Read registers a route the reader's token admits. Only a GET can be one,
// which is checked here rather than trusted: a route that made something
// happen and was open to the reader would hand the usher a command.
func (g *guardedMux) Read(pattern string, h func(http.ResponseWriter, *http.Request)) {
	if !strings.HasPrefix(pattern, "GET ") {
		panic("applifecycle: only a GET can be open to the reader: " + pattern)
	}
	g.mux.HandleFunc(pattern, g.auth(h, true))
}

// HTTPServer exposes app lifecycle operations over HTTP.
type HTTPServer struct {
	Service *Service
	Addr    string
	// Token is the shared secret the director presents. Every request except
	// /healthz must carry it, or the reader's, as a bearer.
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
	// Empty admits nobody as the director rather than everybody. A
	// misconfigured deployment that drops the secret must not quietly become
	// the open API this replaces.
	Token string

	// readToken is a second, separate secret, the usher's. It admits the
	// routes registered with Read and nothing else: no command, no bundle,
	// and not the reads the director's own commands make. The usher answers
	// people's reads of live state from here, and it runs in the edge
	// namespace, facing every console's backend; handing it the director's
	// token would have put delete-backup and purge-app one fault away from a
	// browser.
	//
	// Unset admits nobody as the reader. Set through SetReadToken, because
	// the operator learns it after the listener has started.
	readToken atomic.Pointer[string]
}

// SetReadToken sets the reader's token. An empty one admits nobody.
func (h *HTTPServer) SetReadToken(token string) { h.readToken.Store(&token) }

// matches reports whether presented is the configured token. Constant-time
// compared: a token is a fixed string and an early-exit comparison leaks its
// prefix to anything that can time it. An unset token matches nothing, the
// empty string included.
func matches(presented, configured string) bool {
	if configured == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
}

// authenticated wraps a handler so it is reached only by a caller presenting
// a token that admits it: the director's for every route, the reader's for a
// route registered with Read.
func (h *HTTPServer) authenticated(next http.HandlerFunc, reader bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		readToken := ""
		if p := h.readToken.Load(); p != nil {
			readToken = *p
		}
		if h.Token == "" && readToken == "" {
			writeErr(w, http.StatusServiceUnavailable, errors.New(
				"this API has no token configured and refuses every request; set appLifecycle.tokenSecretName"))
			return
		}
		presented := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		// Both are compared whatever the first says, so the time taken does
		// not tell which of the two a guess came closer to.
		director := matches(presented, h.Token)
		read := matches(presented, readToken)
		switch {
		case director:
		case read && reader && r.Method == http.MethodGet:
		case read:
			writeErr(w, http.StatusForbidden, errors.New("this token reads state; this route is the director's"))
			return
		default:
			writeErr(w, http.StatusUnauthorized, errors.New("this API is the director's and the usher's; present a token of theirs"))
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
	// Nothing below /v1 is open: a tenant's installed apps, its usage and
	// its backups are its own business. Each registrar says which of its
	// routes the reader's token admits as well.
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
