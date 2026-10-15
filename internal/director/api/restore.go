/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/bundlestore"
)

// Restoring a bundle into the tenant it was taken of (sovereignty-concept.md
// §4.3, data-lifecycle.md §4). An action: the operator creates the
// TenantRestore, as it creates the TenantExport of a backup, and nothing is
// committed. The tenant stays as git declares it; only its data is replaced.
//
// The bundle is one the platform holds, named so that the platform finds it
// and not the caller:
//
//   - a backup of the tenant's own, by name: its TenantExport, in the tenant's
//     namespace, records where it was written -- the platform's storage or a
//     destination the tenant's backup policy declared -- and with which
//     credential;
//   - a bundle uploaded to this cluster (POST .../bundles): in the import
//     bucket of the platform's own storage, and nowhere else.
//
// A bundle reference with an address or a credential of its own is refused:
// the restore Jobs reach it with the credential it names, and a tenant's
// administrator naming the platform's or another tenant's would be reading
// with it.
//
// The bundle has to be of this tenant. Its manifest is opened with the key the
// person gives before anything is started, and a bundle of another tenant is
// refused: a restore does not rename, and bringing another tenant's bundle
// here is an import under the name it carries, which is the cluster's
// administrator's. An uploaded bundle is no way round
// that -- whoever made it, it is restored only into the tenant its manifest
// names, which the caller has to administer.
//
// The key travels in the request only: to the operator, which writes it into
// a Secret the restore owns and that goes with it. It is never committed and
// never logged.

type restoreRequest struct {
	Backup string                      `json:"backup,omitempty"`
	Bundle *gentianov1alpha1.BundleRef `json:"bundle,omitempty"`
	// Source names a bundle where the tenant's backups are written, by the
	// source and its prefix there (bundle_sources.go): "storage", its bucket
	// in the cluster's storage, or "destination", its backup policy's. For a
	// bundle the cluster no longer lists as a backup. The operator finds the
	// storage and the keys; the caller names neither.
	Source     *restoreSource  `json:"source,omitempty"`
	Decryption json.RawMessage `json:"decryption"`
	// Apps restores these apps of the bundle and no other; empty is every one.
	Apps []string `json:"apps,omitempty"`
}

// backupName is what a TenantExport can be called.
var backupName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

const restoreBodyHelp = `body must be {"backup": "<name>"}, {"bundle": {"bucket": "...", "prefix": "..."}} or {"source": {"name": "storage|destination", "prefix": "..."}}, with "decryption": {...}`

type restoreSource struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

// which says what the request names, for the operator, or why it is refused.
func (req restoreRequest) which(tenant string) (map[string]any, string) {
	named := 0
	for _, set := range []bool{req.Backup != "", req.Bundle != nil, req.Source != nil} {
		if set {
			named++
		}
	}
	switch {
	case named > 1:
		return nil, "name a backup, a bundle or a source, one of them"
	case req.Source != nil:
		if (req.Source.Name != "storage" && req.Source.Name != "destination") || req.Source.Prefix == "" {
			return nil, `a source restored from is the tenant's own, "storage" or "destination", with the bundle's prefix there`
		}
		return map[string]any{"source": map[string]string{"tenant": tenant, "name": req.Source.Name, "prefix": req.Source.Prefix}}, ""
	case req.Backup != "":
		if !backupName.MatchString(req.Backup) {
			return nil, "that is not the name of a backup"
		}
		return map[string]any{"backup": req.Backup}, ""
	case req.Bundle != nil:
		if !bundlestore.IsImported(req.Bundle) {
			return nil, "a bundle restored by reference is one uploaded to this cluster (POST .../bundles); " +
				"a backup of the tenant's own is named by its name"
		}
		return map[string]any{"bundle": gentianov1alpha1.BundleRef{Bucket: req.Bundle.Bucket, Prefix: req.Bundle.Prefix}}, ""
	}
	return nil, restoreBodyHelp
}

// inspectFor opens the manifest of the bundle the request names, for the
// tenant in the path, and refuses one of another tenant. On refusal it has
// answered, and the manifest is nil.
func (s *Server) inspectFor(w http.ResponseWriter, r *http.Request, c call, req restoreRequest) (map[string]any, *backup.Manifest, json.RawMessage) {
	tenant := r.PathValue("t")
	named, why := req.which(tenant)
	if why != "" {
		s.fail(w, r, http.StatusBadRequest, why)
		return nil, nil, nil
	}
	if len(req.Decryption) == 0 {
		s.fail(w, r, http.StatusBadRequest, restoreBodyHelp)
		return nil, nil, nil
	}
	ask := map[string]any{"decryption": req.Decryption}
	for k, v := range named {
		ask[k] = v
	}
	if req.Backup != "" {
		ask["tenant"] = tenant
	}
	status, body, err := s.cfg.Lifecycle.Do(r.Context(), "/v1/bundles/inspect", c.meta.ActorName(), ask)
	if err != nil {
		s.lifecycleError(w, r, err)
		return nil, nil, nil
	}
	if status != http.StatusOK {
		s.started(w, r, status, body)
		return nil, nil, nil
	}
	var insp struct {
		Info     json.RawMessage  `json:"info,omitempty"`
		Manifest *backup.Manifest `json:"manifest"`
	}
	if err := json.Unmarshal(body, &insp); err != nil || insp.Manifest == nil {
		s.fail(w, r, http.StatusBadGateway, "the operator's inspection did not parse")
		return nil, nil, nil
	}
	if insp.Manifest.Tenant != tenant {
		s.fail(w, r, http.StatusUnprocessableEntity, "this bundle is of another tenant, and a restore puts back only a bundle of the tenant "+
			"restored into; bringing another tenant's bundle here is an import, under the name it carries. Nothing was changed")
		return nil, nil, nil
	}
	return named, insp.Manifest, insp.Info
}

// inspectTenantBundle says what a bundle of this tenant holds, before it is
// restored.
func (s *Server) inspectTenantBundle(w http.ResponseWriter, r *http.Request, c call) {
	var req restoreRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, restoreBodyHelp)
		return
	}
	_, manifest, info := s.inspectFor(w, r, c, req)
	if manifest == nil {
		return
	}
	out := map[string]any{"manifest": manifest}
	if len(info) > 0 {
		out["info"] = info
	}
	s.json(w, http.StatusOK, out)
}

// restoreTenant starts a restore of a bundle of this tenant into it.
func (s *Server) restoreTenant(w http.ResponseWriter, r *http.Request, c call) {
	var req restoreRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, restoreBodyHelp)
		return
	}
	named, manifest, _ := s.inspectFor(w, r, c, req)
	if manifest == nil {
		return
	}
	ask := map[string]any{"decryption": req.Decryption}
	for k, v := range named {
		ask[k] = v
	}
	if len(req.Apps) > 0 {
		ask["apps"] = req.Apps
	}
	status, answer, err := s.cfg.Lifecycle.Do(r.Context(),
		"/v1/tenants/"+url.PathEscape(r.PathValue("t"))+"/actions/restore", c.meta.ActorName(), ask)
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	s.started(w, r, status, answer)
}
