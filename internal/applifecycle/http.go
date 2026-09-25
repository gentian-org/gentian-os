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

package applifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"crypto/subtle"
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
	// can reach the Service, it has writes -- installing an app, setting
	// addons, taking and deleting a backup -- and each took the actor's name
	// from an X-Gentian-Actor header, which is a claim and not a proof. Any
	// pod in any namespace could take a backup and have somebody else's name
	// recorded against it.
	//
	// A shared token rather than the caller's own: verifying that would mean
	// the operator holding the issuer's keys and the authorization graph,
	// which is the director's job and not this process's. The honest end
	// state is that there is no HTTP write here at all, only the director's;
	// until then this is what makes the actor header worth the paper it is
	// written on, because only the director can set it.
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

// Start runs the HTTP server until ctx is cancelled.
func (h *HTTPServer) Start(ctx context.Context) error {
	if h.Service == nil {
		return errors.New("applifecycle service is nil")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Everything below /v1 is the director's, reads included: a tenant's
	// installed apps, its usage and its backups are its own business.
	guarded := &guardedMux{mux: mux, auth: h.authenticated}
	guarded.HandleFunc("GET /v1/tenants/{tenant}/apps", h.handleList)
	guarded.HandleFunc("POST /v1/tenants/{tenant}/apps/{profile}", h.handleInstall)
	guarded.HandleFunc("DELETE /v1/tenants/{tenant}/apps/{profile}", h.handleUninstall)
	guarded.HandleFunc("PUT /v1/tenants/{tenant}/apps/{profile}/addons", h.handleSetAddons)
	h.registerResourceRoutes(guarded)
	h.registerBackupRoutes(guarded)
	h.registerPlatformRoutes(guarded)
	h.registerNotificationRoutes(guarded)

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

func (h *HTTPServer) handleInstall(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	profile := r.PathValue("profile")
	actor := r.Header.Get("X-Gentian-Actor")
	if actor == "" {
		actor = "app-lifecycle-api"
	}
	wait := r.URL.Query().Get("wait") == "true" || r.URL.Query().Get("wait") == "1"
	provision := r.URL.Query().Get("provision") == "true" || r.URL.Query().Get("provision") == "1"
	result, err := h.Service.Install(r.Context(), InstallRequest{
		Tenant:    tenant,
		Profile:   profile,
		Actor:     actor,
		Wait:      wait,
		Provision: provision,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *HTTPServer) handleUninstall(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	profile := r.PathValue("profile")
	purge := r.URL.Query().Get("purge") == "true" || r.URL.Query().Get("purge") == "1"
	actor := r.Header.Get("X-Gentian-Actor")
	if actor == "" {
		actor = "app-lifecycle-api"
	}
	result, err := h.Service.Uninstall(r.Context(), UninstallRequest{
		Tenant:  tenant,
		Profile: profile,
		Purge:   purge,
		Actor:   actor,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *HTTPServer) handleSetAddons(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	profile := r.PathValue("profile")
	actor := r.Header.Get("X-Gentian-Actor")
	if actor == "" {
		actor = "app-lifecycle-api"
	}

	// PUT, not PATCH: the body is the complete selection. A partial "add these"
	// verb would make deselection unexpressible, and deselecting to nothing is a
	// selection the activation script has to see.
	var body struct {
		Addons []string `json:"addons"`
		// Provision mirrors the app-level flag: install and grant access to all
		// existing tenant users, rather than install and leave access to group
		// assignment. It applies to every addon in the request.
		Provision bool `json:"provision"`
		// ProvisionFor is the same choice made per addon, which is how the store
		// asks it — Install and Provision are separate buttons on each row. Given
		// both, this one decides; an addon it omits is installed and not granted.
		ProvisionFor []string `json:"provisionFor"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	result, err := h.Service.SetAddons(r.Context(), SetAddonsRequest{
		Tenant:       tenant,
		Profile:      profile,
		Addons:       body.Addons,
		Provision:    body.Provision,
		ProvisionFor: body.ProvisionFor,
		Actor:        actor,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"detail": strings.TrimSpace(err.Error())})
}
