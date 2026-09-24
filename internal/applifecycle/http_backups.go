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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What the cluster knows about backups, for the director to relay.
//
// Reads only, like the resources routes beside them and for the same reason:
// a policy is declared state and reaches the cluster as a commit the director
// makes to git, so there is no write here to call. What the console needs
// that git cannot answer is the state — which exports exist, what each one
// did, what a policy resolves to once inheritance is applied, and when the
// next scheduled run is — and that is what this serves.
func (h *HTTPServer) registerBackupRoutes(mux *http.ServeMux) {
	// Reads of state.
	mux.HandleFunc("GET /v1/tenants/{tenant}/backups", h.handleBackups)
	mux.HandleFunc("GET /v1/tenants/{tenant}/backups/{name}", h.handleBackup)
	mux.HandleFunc("GET /v1/tenants/{tenant}/backup-policy", h.handleTenantBackupPolicy)
	mux.HandleFunc("GET /v1/tenants/{tenant}/backup-schedules", h.handleBackupSchedules)
	mux.HandleFunc("GET /v1/backup-policy", h.handleClusterBackupPolicy)
	mux.HandleFunc("GET /v1/backup-schedules", h.handleAllBackupSchedules)

	// Actions. Under /actions/ and always POST, because they are neither a
	// read nor a change to what the cluster should be: they make something
	// happen now, once. A policy -- what should be true of every run -- is
	// declared state and arrives as a commit the director makes to git;
	// there is no endpoint for it here, and that absence is the design.
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/backup", h.handleStartBackup)
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/delete-backup", h.handleDeleteBackup)
}

// actorOf reads who asked.
//
// The director puts the person there, having verified their token and checked
// the relation. It is a claim, not a proof: this API authenticates nobody, and
// anything that can reach the Service can assert any name — which is equally
// true of the app install and uninstall writes that have been here all along.
// Making it a proof is its own piece of work (S7A.16), and until it is done
// the name here is only as good as who can reach the port.
func actorOf(r *http.Request) string {
	if actor := r.Header.Get("X-Gentian-Actor"); actor != "" {
		return actor
	}
	return "app-lifecycle-api"
}

func (h *HTTPServer) handleStartBackup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string                              `json:"name"`
		Apps        []string                            `json:"apps"`
		Recipients  []string                            `json:"recipients"`
		Destination *gentianov1alpha1.ExportDestination `json:"destination"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	started, err := h.Service.StartBackup(r.Context(), StartBackupRequest{
		Tenant: r.PathValue("tenant"), Name: body.Name, Actor: actorOf(r),
		Apps: body.Apps, Recipients: body.Recipients, Destination: body.Destination,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, started)
}

func (h *HTTPServer) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	started, err := h.Service.DeleteBackup(r.Context(), r.PathValue("tenant"), body.Name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusAccepted, started)
}

func (h *HTTPServer) handleBackups(w http.ResponseWriter, r *http.Request) {
	backups, err := h.Service.Backups(r.Context(), r.PathValue("tenant"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": r.PathValue("tenant"), "backups": backups})
}

func (h *HTTPServer) handleBackup(w http.ResponseWriter, r *http.Request) {
	one, err := h.Service.Backup(r.Context(), r.PathValue("tenant"), r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, one)
}

func (h *HTTPServer) handleTenantBackupPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := h.Service.BackupPolicy(r.Context(), "tenant", r.PathValue("tenant"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (h *HTTPServer) handleClusterBackupPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := h.Service.BackupPolicy(r.Context(), "cluster", "")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (h *HTTPServer) handleBackupSchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := h.Service.BackupSchedules(r.Context(), r.PathValue("tenant"), false)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": r.PathValue("tenant"), "schedules": schedules})
}

// handleAllBackupSchedules answers for every tenant, for the cluster's view.
func (h *HTTPServer) handleAllBackupSchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := h.Service.BackupSchedules(r.Context(), "", true)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": schedules})
}

// registerPlatformRoutes serves the reads behind the console's remaining
// screens. Reads only, like the rest of this API: what an app may consume is
// declared in git, what the cluster permits to escape the default posture is
// the operator chart's, and a customisation record is written where the
// customisation is.
func (h *HTTPServer) registerPlatformRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tenants/{tenant}/integrations", h.handleIntegrations)
	mux.HandleFunc("GET /v1/platform-security", h.handlePlatformSecurity)
	mux.HandleFunc("GET /v1/customizations", h.handleCustomizationDebt)
}

func (h *HTTPServer) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	overview, err := h.Service.Integrations(r.Context(), r.PathValue("tenant"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (h *HTTPServer) handlePlatformSecurity(w http.ResponseWriter, r *http.Request) {
	policy, err := h.Service.PlatformSecurity(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (h *HTTPServer) handleCustomizationDebt(w http.ResponseWriter, r *http.Request) {
	report, err := h.Service.CustomizationDebtReport(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// registerNotificationRoutes serves the notices an administrator publishes.
//
// A read of what has been said, and an action that says something. The table
// belongs to the desktop, which is where people read them; this writes into
// it because the operator already resolves that database, and a console that
// kept its own copy would be a second place a notice could exist.
func (h *HTTPServer) registerNotificationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tenants/{tenant}/notifications", h.handleNotifications)
	mux.HandleFunc("POST /v1/tenants/{tenant}/actions/notify", h.handlePublishNotification)
}

func (h *HTTPServer) handleNotifications(w http.ResponseWriter, r *http.Request) {
	notices, err := h.Service.Notifications(r.Context(), r.PathValue("tenant"))
	if err != nil {
		writeErr(w, notificationStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": r.PathValue("tenant"), "notifications": notices})
}

func (h *HTTPServer) handlePublishNotification(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title     string                `json:"title"`
		Body      string                `json:"body"`
		Severity  string                `json:"severity"`
		Audience  *NotificationAudience `json:"audience"`
		LinkURL   string                `json:"linkUrl"`
		LinkLabel string                `json:"linkLabel"`
		ExpiresAt int64                 `json:"expiresAt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	notice, err := h.Service.PublishNotification(r.Context(), PublishNotificationRequest{
		Tenant: r.PathValue("tenant"), Actor: actorOf(r),
		Title: body.Title, Body: body.Body, Severity: body.Severity, Audience: body.Audience,
		LinkURL: body.LinkURL, LinkLabel: body.LinkLabel, ExpiresAt: body.ExpiresAt,
	})
	if err != nil {
		writeErr(w, notificationStatus(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, notice)
}

// notificationStatus distinguishes the one failure a caller can act on: the
// desktop has not created its table yet, which is a 503 rather than a 500 —
// it will work once the desktop has started, and nothing is broken.
func notificationStatus(err error) int {
	if errors.Is(err, ErrNoNotificationStore) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}
