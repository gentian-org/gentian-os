/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Importing a tenant: Create plus Restore (sovereignty-concept.md §4.3).
//
// The bundle is uploaded or named; its manifest is read with the key the
// person gives; the tenant is declared from the manifest's spec as one
// commit; and once the operator reports it Ready, with its apps up, a
// restore fills the shells from the bundle. Everything after the commit runs
// in the background and the status route says where it is -- an import is
// minutes to hours, and no request waits that long.

// ImportStatus is where one import stands.
type ImportStatus struct {
	Tenant  string `json:"tenant"`
	Phase   string `json:"phase"` // declared | provisioning | restoring | ready | failed
	Message string `json:"message,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Restore string `json:"restore,omitempty"`
	// Bundle names what was imported, for the record.
	Bundle gentianov1alpha1.BundleRef `json:"bundle"`
	// PasswordResetRequired is the restore's word: members come back without
	// credentials, which no bundle carries.
	PasswordResetRequired bool `json:"passwordResetRequired,omitempty"`
	// Complete, NotRestored and Notes are the restore's too: whether
	// everything the bundle holds was put back, what was not and why, and
	// what a restore never brings back.
	Complete    *bool             `json:"complete,omitempty"`
	NotRestored []json.RawMessage `json:"notRestored,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
}

var (
	importPoll = 10 * time.Second
	importWait = 2 * time.Hour
)

type importRequest struct {
	Bundle     gentianov1alpha1.BundleRef `json:"bundle"`
	Decryption json.RawMessage            `json:"decryption"`
	// Name overrides the tenant name the manifest carries: for bringing a
	// bundle up beside the tenant it came from.
	Name string `json:"name,omitempty"`
}

// uploadBundle streams the request body to the operator, which lays it out
// in the cluster's storage and answers with where.
func (s *Server) uploadBundle(w http.ResponseWriter, r *http.Request, _ call) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	status, body, err := s.cfg.Lifecycle.Upload(r.Context(), "/v1/bundles", r.Header.Get("Content-Type"), r.Body)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "the operator's API did not answer")
		return
	}
	s.started(w, r, status, body)
}

// inspectBundle opens the manifest and says what the bundle holds, so a
// person can see what they are about to import before anything is declared.
func (s *Server) inspectBundle(w http.ResponseWriter, r *http.Request, c call) {
	var req importRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"bundle": {...}, "decryption": {...}}`)
		return
	}
	status, body, err := s.cfg.Lifecycle.Do(r.Context(), "/v1/bundles/inspect", c.meta.ActorName(),
		map[string]any{"bundle": req.Bundle, "decryption": req.Decryption})
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "the operator's API did not answer")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// importTenant declares the tenant from the bundle's manifest and starts
// watching for the moment a restore can begin.
func (s *Server) importTenant(w http.ResponseWriter, r *http.Request, c call) {
	var req importRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"bundle": {...}, "decryption": {...}, "name": "<optional>"}`)
		return
	}
	status, body, err := s.cfg.Lifecycle.Do(r.Context(), "/v1/bundles/inspect", c.meta.ActorName(),
		map[string]any{"bundle": req.Bundle, "decryption": req.Decryption})
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "the operator's API did not answer")
		return
	}
	if status != http.StatusOK {
		s.started(w, r, status, body)
		return
	}
	var insp struct {
		Manifest *backup.Manifest `json:"manifest"`
	}
	if err := json.Unmarshal(body, &insp); err != nil || insp.Manifest == nil {
		s.fail(w, r, http.StatusBadGateway, "the operator's inspection did not parse")
		return
	}
	name := insp.Manifest.Tenant
	if req.Name != "" {
		name = req.Name
	}
	if !gitops.ValidName(name) {
		s.fail(w, r, http.StatusBadRequest, "the tenant name is not a DNS label")
		return
	}
	if _, busy := s.imports.Load(name); busy {
		if st, ok := s.imports.Load(name); ok && !importDone(st.(ImportStatus)) {
			s.fail(w, r, http.StatusConflict, "an import of this tenant is already under way")
			return
		}
	}
	origin := fmt.Sprintf("export %s of %s", insp.Manifest.Export, insp.Manifest.CreatedAt)
	res, err := s.cfg.Repo.DeclareTenant(r.Context(), name, insp.Manifest.TenantSpec, origin, c.meta)
	if errors.Is(err, gitops.ErrTenantExists) {
		s.fail(w, r, http.StatusConflict, "a tenant of that name already exists; restore into it instead, or import under another name")
		return
	}
	if errors.Is(err, gitops.ErrSingleTenancy) {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	st := ImportStatus{Tenant: name, Phase: "declared", Commit: res.Commit, Bundle: req.Bundle,
		Message: "the tenant is committed; the operator provisions it next"}
	s.imports.Store(name, st)
	go s.finishImport(name, req, c.meta.ActorName())
	s.json(w, http.StatusAccepted, st)
}

func (s *Server) importStatus(w http.ResponseWriter, r *http.Request, _ call) {
	st, ok := s.imports.Load(r.PathValue("t"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, "no import of this tenant is known to this director")
		return
	}
	s.json(w, http.StatusOK, st)
}

func importDone(st ImportStatus) bool { return st.Phase == "ready" || st.Phase == "failed" }

func (s *Server) setImport(name string, mutate func(*ImportStatus)) {
	st, _ := s.imports.Load(name)
	cur, _ := st.(ImportStatus)
	mutate(&cur)
	s.imports.Store(name, cur)
}

// finishImport waits for the operator, then starts the restore and watches
// it to its end.
func (s *Server) finishImport(name string, req importRequest, actor string) {
	ctx, cancel := context.WithTimeout(context.Background(), importWait)
	defer cancel()
	fail := func(msg string) {
		s.setImport(name, func(st *ImportStatus) { st.Phase = "failed"; st.Message = msg })
		s.cfg.Log.Error("import failed", "tenant", name, "error", msg)
	}
	s.setImport(name, func(st *ImportStatus) {
		st.Phase = "provisioning"
		st.Message = "waiting for the operator to provision the tenant and its apps"
	})
	for {
		ready, msg, err := s.tenantProvisioned(ctx, name)
		if err != nil {
			s.cfg.Log.Warn("asking the cluster about an import", "tenant", name, "error", err.Error())
		} else if msg != "" {
			s.setImport(name, func(st *ImportStatus) { st.Message = msg })
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			fail("the tenant was not provisioned within " + importWait.String() + "; the manifest is committed, so a restore can be started by hand once it is")
			return
		case <-time.After(importPoll):
		}
	}
	status, body, err := s.cfg.Lifecycle.Do(ctx, "/v1/tenants/"+url.PathEscape(name)+"/actions/restore", actor,
		map[string]any{"bundle": req.Bundle, "decryption": req.Decryption})
	if err != nil || status/100 != 2 {
		fail("the restore could not be started: " + strings.TrimSpace(string(body)))
		return
	}
	var started struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body, &started)
	s.setImport(name, func(st *ImportStatus) {
		st.Phase = "restoring"
		st.Restore = started.Name
		st.Message = "restoring the bundle into the tenant"
	})
	for {
		code, body, err := s.cfg.Lifecycle.Get(ctx, "/v1/tenants/"+url.PathEscape(name)+"/restores/"+url.PathEscape(started.Name), nil)
		if err == nil && code == http.StatusOK {
			var rs struct {
				Phase                 string            `json:"phase"`
				Message               string            `json:"message"`
				PasswordResetRequired bool              `json:"passwordResetRequired"`
				Complete              *bool             `json:"complete"`
				NotRestored           []json.RawMessage `json:"notRestored"`
				Notes                 []string          `json:"notes"`
			}
			if json.Unmarshal(body, &rs) == nil {
				switch rs.Phase {
				case "Ready":
					s.setImport(name, func(st *ImportStatus) {
						st.Phase = "ready"
						st.Message = rs.Message
						st.PasswordResetRequired = rs.PasswordResetRequired
						st.Complete, st.NotRestored, st.Notes = rs.Complete, rs.NotRestored, rs.Notes
					})
					s.cfg.Log.Info("tenant imported", "tenant", name, "restore", started.Name)
					return
				case "Failed":
					fail("the restore failed: " + rs.Message)
					return
				default:
					if rs.Message != "" {
						s.setImport(name, func(st *ImportStatus) { st.Message = rs.Message })
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			fail("the restore did not finish within " + importWait.String())
			return
		case <-time.After(importPoll):
		}
	}
}

// tenantProvisioned is whether the live Tenant is Ready and every app it
// lists is up -- the restore pauses each app, so each has to exist first.
func (s *Server) tenantProvisioned(ctx context.Context, name string) (bool, string, error) {
	code, body, err := s.cfg.Lifecycle.Get(ctx, "/v1/tenants/"+url.PathEscape(name), nil)
	if err != nil {
		return false, "", err
	}
	if code != http.StatusOK {
		return false, "", fmt.Errorf("the operator answered %d", code)
	}
	var state struct {
		Exists bool   `json:"exists"`
		Phase  string `json:"phase"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return false, "", err
	}
	if !state.Exists {
		return false, "waiting for Argo CD to apply the tenant", nil
	}
	if state.Phase != "Ready" {
		return false, "the tenant is " + state.Phase, nil
	}
	code, body, err = s.cfg.Lifecycle.Get(ctx, "/v1/tenants/"+url.PathEscape(name)+"/apps/status", nil)
	if err != nil || code != http.StatusOK {
		return false, "", fmt.Errorf("apps status: %d %v", code, err)
	}
	var apps struct {
		Apps []struct {
			Profile           string   `json:"profile"`
			Ready             bool     `json:"ready"`
			Phase             string   `json:"phase"`
			Failure           string   `json:"failure"`
			PendingPrivileges []string `json:"pendingPrivileges"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(body, &apps); err != nil {
		return false, "", err
	}
	var waiting []string
	for _, a := range apps.Apps {
		if a.Ready {
			continue
		}
		why := a.Phase
		if a.Failure != "" {
			why = a.Failure
		}
		if len(a.PendingPrivileges) > 0 {
			why = "privileges pending: " + strings.Join(a.PendingPrivileges, ", ")
		}
		waiting = append(waiting, a.Profile+" ("+why+")")
	}
	if len(waiting) > 0 {
		return false, "waiting for apps: " + strings.Join(waiting, "; "), nil
	}
	return true, "", nil
}
