/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing limitations and the License.
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// HTTPServer exposes app lifecycle operations over HTTP.
type HTTPServer struct {
	Service *Service
	Addr    string
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
	mux.HandleFunc("GET /v1/tenants/{tenant}/apps", h.handleList)
	mux.HandleFunc("POST /v1/tenants/{tenant}/apps/{profile}", h.handleInstall)
	mux.HandleFunc("DELETE /v1/tenants/{tenant}/apps/{profile}", h.handleUninstall)

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
	result, err := h.Service.Install(r.Context(), InstallRequest{
		Tenant:  tenant,
		Profile: profile,
		Actor:   actor,
		Wait:    wait,
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"detail": strings.TrimSpace(err.Error())})
}
