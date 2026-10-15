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
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Where bundles are read from, other than an upload (data-lifecycle.md §4).
//
// The cluster's sources are storage outside the cluster, declared by its
// administrator: the address is a commit here, the keys are set at the
// custodian as the requirement bundle-source-<name>, which the operator makes
// from the declaration. A tenant's own sources are where its backups are
// written: its bucket in the cluster's storage, and its backup policy's
// destination. Nothing here holds or passes a key.
//
// A bundle is named by its source and its prefix there, never by an address
// or a Secret: the operator finds both. Listing reads each bundle's plaintext
// header; fetching copies one into the cluster's import storage by a Job
// whose only way out is the source's address, for as long as it runs; the
// fetched bundle is then downloaded, inspected, imported or restored as an
// upload is.
//
// Authorization follows the source. The cluster's are the right an import
// needs, can_configure; a tenant's are listed under the relation its restore
// is asked under, can_set_policy, and under can_configure by the cluster's
// administrator, as a restore is.

// sourceAnswer is one source as a console lists it: what git declares, with
// what the cluster made of it.
type sourceAnswer struct {
	gitops.DeclaredBundleSource
	// Declared is false for a source the cluster holds and git no longer
	// declares; Applied false for one git declares and the cluster does not
	// hold yet.
	Declared bool `json:"declared"`
	Applied  bool `json:"applied"`
	// CredentialName is the requirement the keys are set at, at the
	// custodian.
	CredentialName string `json:"credentialName"`
	Ready          bool   `json:"ready"`
	Message        string `json:"message,omitempty"`
}

type liveSource struct {
	Name            string `json:"name"`
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix"`
	Region          string `json:"region"`
	AddressingStyle string `json:"addressingStyle"`
	Description     string `json:"description"`
	Ready           bool   `json:"ready"`
	Message         string `json:"message"`
}

// listClusterSources answers the cluster's declared sources, each with
// whether its keys are there.
func (s *Server) listClusterSources(w http.ResponseWriter, r *http.Request, _ call) {
	declared, err := s.cfg.Repo.BundleSources(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	status, body, err := s.cfg.Lifecycle.Get(r.Context(), "/v1/bundle-sources", nil)
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	if status != http.StatusOK {
		s.started(w, r, status, body)
		return
	}
	var live struct {
		Sources []liveSource `json:"sources"`
	}
	if err := json.Unmarshal(body, &live); err != nil {
		s.fail(w, r, http.StatusBadGateway, "the operator's list of sources did not parse")
		return
	}
	byName := map[string]liveSource{}
	for _, l := range live.Sources {
		byName[l.Name] = l
	}
	out := make([]sourceAnswer, 0, len(declared)+len(live.Sources))
	for _, d := range declared {
		a := sourceAnswer{DeclaredBundleSource: d, Declared: true, CredentialName: backup.SourceCredentialName(d.Name)}
		if l, ok := byName[d.Name]; ok {
			a.Applied, a.Ready, a.Message = true, l.Ready, l.Message
			delete(byName, d.Name)
		} else {
			a.Message = "declared; Argo CD applies it on its next sync"
		}
		out = append(out, a)
	}
	for _, l := range live.Sources {
		if _, left := byName[l.Name]; !left {
			continue
		}
		out = append(out, sourceAnswer{
			DeclaredBundleSource: gitops.DeclaredBundleSource{Name: l.Name, BundleSourceDeclaration: gitops.BundleSourceDeclaration{
				Endpoint: l.Endpoint, Bucket: l.Bucket, Prefix: l.Prefix, Region: l.Region,
				AddressingStyle: l.AddressingStyle, Description: l.Description,
			}},
			Applied: true, CredentialName: backup.SourceCredentialName(l.Name), Ready: l.Ready,
			Message: "the cluster holds this source and git does not declare it; removing it removes it from the cluster",
		})
	}
	s.json(w, http.StatusOK, map[string]any{"sources": out})
}

// declareClusterSource commits one source. The keys are not in the body and
// cannot be: they are set at the custodian, once the source is applied.
func (s *Server) declareClusterSource(w http.ResponseWriter, r *http.Request, c call) {
	name := r.PathValue("name")
	var body gitops.BundleSourceDeclaration
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"endpoint": "https://…", "bucket": "…", "prefix": "…", "region": "…", "addressingStyle": "path|virtualHost"}`)
		return
	}
	res, err := s.cfg.Repo.DeclareBundleSource(r.Context(), name, body, c.meta)
	if errors.Is(err, gitops.ErrInvalidBundleSource) {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	answer := map[string]any{"status": res.Status, "name": name, "credentialName": backup.SourceCredentialName(name)}
	if !res.Changed {
		s.json(w, http.StatusOK, answer)
		return
	}
	answer["commit"] = res.Commit
	answer["message"] = "committed to the deployments repository; Argo CD applies it on the next sync, " +
		"and its keys are set at the custodian as " + backup.SourceCredentialName(name) + " once it has"
	s.json(w, http.StatusAccepted, answer)
}

// removeClusterSource stops declaring a source, and has the cluster delete
// it: the cluster's declarations are synced without pruning.
func (s *Server) removeClusterSource(w http.ResponseWriter, r *http.Request, c call) {
	name := r.PathValue("name")
	res, err := s.cfg.Repo.RemoveBundleSource(r.Context(), name, c.meta)
	switch {
	case errors.Is(err, gitops.ErrInvalidName):
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, gitops.ErrBundleSourceNotFound):
		// Nothing to commit; the cluster may still hold one left behind.
	case err != nil:
		s.repoError(w, r, err)
		return
	}
	status, body, err := s.cfg.Lifecycle.Do(r.Context(), "/v1/bundle-sources/"+url.PathEscape(name)+"/actions/retire",
		c.meta.ActorName(), map[string]any{})
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	if status/100 != 2 {
		s.started(w, r, status, body)
		return
	}
	s.json(w, http.StatusAccepted, map[string]any{"status": res.Status, "name": name, "commit": res.Commit, "deleted": true,
		"message": "the declaration is removed and the cluster no longer reads from this source. Its keys stay in the vault until they are overwritten"})
}

// relay answers what the operator answered, as it answered it.
func (s *Server) relay(w http.ResponseWriter, r *http.Request, status int, body []byte, err error) {
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	if status/100 != 2 {
		s.started(w, r, status, body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) clusterSourceBundles(w http.ResponseWriter, r *http.Request, _ call) {
	status, body, err := s.cfg.Lifecycle.Get(r.Context(), "/v1/bundle-sources/"+url.PathEscape(r.PathValue("name"))+"/bundles", nil)
	s.relay(w, r, status, body, err)
}

func (s *Server) tenantSources(w http.ResponseWriter, r *http.Request, _ call) {
	status, body, err := s.cfg.Lifecycle.Get(r.Context(), "/v1/tenants/"+url.PathEscape(r.PathValue("t"))+"/bundle-sources", nil)
	s.relay(w, r, status, body, err)
}

func (s *Server) tenantSourceBundles(w http.ResponseWriter, r *http.Request, _ call) {
	status, body, err := s.cfg.Lifecycle.Get(r.Context(),
		"/v1/tenants/"+url.PathEscape(r.PathValue("t"))+"/bundle-sources/"+url.PathEscape(r.PathValue("name"))+"/bundles", nil)
	s.relay(w, r, status, body, err)
}

type fetchRequest struct {
	Prefix string `json:"prefix"`
}

// fetchFrom starts a fetch of one bundle at a source: the cluster's when
// tenant is empty, else that tenant's. The source is the route's, never the
// body's.
func (s *Server) fetchFrom(w http.ResponseWriter, r *http.Request, c call, tenant string) {
	var body fetchRequest
	if err := decode(r, &body); err != nil || body.Prefix == "" {
		s.fail(w, r, http.StatusBadRequest, `body must be {"prefix": "<the bundle's prefix at the source>"}`)
		return
	}
	status, answer, err := s.cfg.Lifecycle.Do(r.Context(), "/v1/bundle-fetches", c.meta.ActorName(), map[string]any{
		"source": map[string]string{"tenant": tenant, "name": r.PathValue("name"), "prefix": body.Prefix},
	})
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	s.started(w, r, status, answer)
}

func (s *Server) fetchFromClusterSource(w http.ResponseWriter, r *http.Request, c call) {
	s.fetchFrom(w, r, c, "")
}

func (s *Server) fetchFromTenantSource(w http.ResponseWriter, r *http.Request, c call) {
	s.fetchFrom(w, r, c, r.PathValue("t"))
}

func (s *Server) bundleFetch(w http.ResponseWriter, r *http.Request, _ call) {
	status, body, err := s.cfg.Lifecycle.Get(r.Context(), "/v1/bundle-fetches/"+url.PathEscape(r.PathValue("f")), nil)
	s.relay(w, r, status, body, err)
}

// importedBundleDownload relays a bundle of the cluster's import storage --
// an upload or a fetch -- as one file, as a backup's download is relayed.
func (s *Server) importedBundleDownload(w http.ResponseWriter, r *http.Request, _ call) {
	resp, err := s.cfg.Lifecycle.Stream(r.Context(), "/v1/bundles/"+url.PathEscape(r.PathValue("prefix"))+"/download")
	if err != nil {
		s.cfg.Log.ErrorContext(r.Context(), "app-lifecycle API unreachable", "request_id", reqID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusBadGateway, "the operator's API did not answer")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range []string{"Content-Type", "Content-Disposition", "Content-Length"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
