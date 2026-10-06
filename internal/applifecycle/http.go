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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// router is what the route registrars take, so the guard cannot be forgotten
// by registering against the bare mux: the only implementation here wraps
// every handler.
//
// It has two ways to register and a route says which it is. HandleFunc is the
// director's alone: every command, and the reads only the director's own
// commands need. Read is a read of state the usher answers a person with; the
// director's identity admits it and so does the usher's.
type router interface {
	HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request))
	Read(pattern string, h func(http.ResponseWriter, *http.Request))
}

// guardedMux registers every handler behind the caller check.
type guardedMux struct {
	mux *http.ServeMux
	// auth wraps a handler; reader says whether the reader's identity admits
	// it.
	auth func(next http.HandlerFunc, reader bool) http.HandlerFunc
}

func (g *guardedMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	g.mux.HandleFunc(pattern, g.auth(h, false))
}

// Read registers a route the reader's identity admits. Only a GET can be one,
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
	// Auth says who is calling. Every request except /healthz must carry a
	// ServiceAccount token issued for this listener's audience, and is
	// admitted by the identity the API server reports for it (auth.go): the
	// director's for every route, the usher's for the routes registered with
	// Read and for nothing else -- no command, no bundle, and not the reads
	// the director's own commands make.
	//
	// This API takes and deletes backups and purges an app's data, and each
	// command records the actor named in an X-Gentian-Actor header. That
	// header is a claim; what makes it worth recording is that only the
	// director's identity is admitted to a route that reads it. The operator
	// verifies no person here: that is the director's job and the usher's,
	// done before either calls.
	//
	// Nil, or configured with nobody to admit, refuses every request rather
	// than serving any: a deployment that lost its configuration must not
	// become an open API.
	Auth *CallerAuth
}

// bearer returns the token a request presents, or "".
func bearer(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(token)
}

// authenticated wraps a handler so it is reached only by a caller whose
// identity admits it: the director's for every route, the reader's for a
// route registered with Read.
func (h *HTTPServer) authenticated(next http.HandlerFunc, reader bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.Auth.configured() {
			writeErr(w, http.StatusServiceUnavailable, errors.New(
				"this API has no caller configured and refuses every request; set appLifecycle.audience and enable the director"))
			return
		}
		presented := bearer(r)
		if presented == "" {
			writeErr(w, http.StatusUnauthorized, errors.New("this API is the director's and the usher's; present a ServiceAccount token of theirs"))
			return
		}
		username, err := h.Auth.identify(r.Context(), presented)
		callers := h.Auth.callers
		switch {
		case errors.Is(err, errNotAuthenticated):
			writeErr(w, http.StatusUnauthorized, errors.New("this API is the director's and the usher's; the token presented is not a valid one for it"))
			return
		case err != nil:
			// The API server could not be asked. Refused, not admitted: the
			// alternative is a listener that opens when the control plane is
			// unwell. The error names no token; identify never puts one in.
			ctrllog.FromContext(r.Context()).WithName("app-lifecycle").Error(err, "a caller could not be identified and was refused")
			writeErr(w, http.StatusServiceUnavailable, errors.New("the caller could not be identified: the API server did not answer"))
			return
		case username == callers.Director:
		case username == callers.Reader && reader && r.Method == http.MethodGet:
		case username == callers.Reader:
			writeErr(w, http.StatusForbidden, errors.New("this identity reads state; this route is the director's"))
			return
		default:
			writeErr(w, http.StatusForbidden, errors.New("this API is the director's and the usher's; this identity is neither"))
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
	// routes the reader's identity admits as well.
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
