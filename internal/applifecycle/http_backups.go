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
	"net/http"
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
	mux.HandleFunc("GET /v1/tenants/{tenant}/backups", h.handleBackups)
	mux.HandleFunc("GET /v1/tenants/{tenant}/backups/{name}", h.handleBackup)
	mux.HandleFunc("GET /v1/tenants/{tenant}/backup-policy", h.handleTenantBackupPolicy)
	mux.HandleFunc("GET /v1/tenants/{tenant}/backup-schedules", h.handleBackupSchedules)
	mux.HandleFunc("GET /v1/backup-policy", h.handleClusterBackupPolicy)
	mux.HandleFunc("GET /v1/backup-schedules", h.handleAllBackupSchedules)
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
