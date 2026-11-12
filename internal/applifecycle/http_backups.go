/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gentian-org/gentian-os/internal/usagestatistics"
)

// What the cluster knows about backups, for the director to relay.
//
// Reads only, like the resources routes beside them and for the same reason:
// a policy is declared state and reaches the cluster as a commit the director
// makes to git, so there is no write here to call. What the console needs
// that git cannot answer is the state — which exports exist, what each one
// did, and what a policy resolves to once inheritance is applied — and that
// is what this serves.
func (h *HTTPServer) registerBackupRoutes(mux router) {
	// Reads of state.
	mux.Read("GET /v1/tenants/{tenant}/backups", h.handleBackups)
	mux.Read("GET /v1/tenants/{tenant}/backups/{name}", h.handleBackup)
	// The bundle itself, as one file. Not JSON: a tar of the artifacts, for
	// the person who wants their data in their hands. The director's alone:
	// the reader's identity lists backups and does not fetch one.
	mux.HandleFunc("GET /v1/tenants/{tenant}/backups/{name}/download", h.handleBundleDownload)
	mux.Read("GET /v1/tenants/{tenant}/backup-policy", h.handleTenantBackupPolicy)
	mux.Read("GET /v1/backup-policy", h.handleClusterBackupPolicy)

	// Taking a backup and deleting one are acts, done once on a call, and
	// are the dispatcher's (internal/dispatcher/acts.go). A policy -- what
	// should be true of every run -- is declared state and arrives as a
	// commit the director makes to git; there is no endpoint for either
	// here.
}

// actorOf reads who asked.
//
// The director puts the person there, having verified their token and checked
// the relation. The routes that read it admit the director's identity alone
// (authenticated), so the name is the director's word and nobody else's.
func actorOf(r *http.Request) string {
	if actor := r.Header.Get("X-Gentian-Actor"); actor != "" {
		return actor
	}
	return "app-lifecycle-api"
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

// registerPlatformRoutes serves the reads behind the console's remaining
// screens. Reads only, like the rest of this API: what an app may consume is
// declared in git, what the cluster permits to escape the default posture is
// the operator chart's, and a customization record is written where the
// customization is.
func (h *HTTPServer) registerPlatformRoutes(mux router) {
	mux.Read("GET /v1/tenants/{tenant}/integrations", h.handleIntegrations)
	mux.Read("GET /v1/platform-security", h.handlePlatformSecurity)
	mux.Read("GET /v1/customizations", h.handleCustomizationDebt)
	// What this cluster last told the receiving endpoint about itself, and
	// what became of it. Open to the reader because it is the cluster's own
	// statement, shown to whoever may audit the cluster.
	mux.Read("GET /v1/usage-statistics", h.handleUsageStatistics)
	// The release the Cluster claim declares and what the operator made of
	// it, for the director, which cannot read the claim itself.
	h.registerReleaseRoutes(mux)
}

func (h *HTTPServer) handleUsageStatistics(w http.ResponseWriter, r *http.Request) {
	view, err := usagestatistics.Read(r.Context(), h.Service.client, h.Service.opts.OperatorNamespace, h.Service.opts.UsageStatistics)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
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
func (h *HTTPServer) registerNotificationRoutes(mux router) {
	mux.Read("GET /v1/tenants/{tenant}/notifications", h.handleNotifications)
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
