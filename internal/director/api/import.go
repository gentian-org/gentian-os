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
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// Importing a tenant: Create plus Restore (sovereignty-concept.md §4.3).
//
// The bundle is uploaded or named; its manifest is read with the key the
// person gives; the definition of every app the bundle's tenant lists is
// made sure of -- on the cluster already, or fetched at the build the bundle
// records -- or the import is refused before anything is changed; the tenant
// is declared from the manifest's settings as one commit, by the function a
// created tenant is committed by; and once the operator reports it Ready,
// with its apps up, a restore fills the shells from the bundle. Everything
// after the commit runs in the background and the status route says where it
// is -- an import is minutes to hours, and no request waits that long.
//
// The commit carries the record of the import (gitops.PendingImport), which
// is removed when the import has finished. A director that restarts in
// between finds it (ResumeImports) and goes on: it watches the restore when
// one was started, and otherwise says it needs the bundle's key again, which
// was given with the request and is kept in memory only.

// ImportStatus is where one import stands.
type ImportStatus struct {
	Tenant  string `json:"tenant"`
	Phase   string `json:"phase"` // declared | provisioning | awaiting-key | restoring | ready | failed
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
// watching for the moment a restore can begin. Asked again for an import that
// has not finished, with the same bundle, it goes on with that import: that
// is how an import is given its key again after the director restarted.
func (s *Server) importTenant(w http.ResponseWriter, r *http.Request, c call) {
	ctx := r.Context()
	var req importRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"bundle": {...}, "decryption": {...}, "name": "<optional>"}`)
		return
	}
	status, body, err := s.cfg.Lifecycle.Do(ctx, "/v1/bundles/inspect", c.meta.ActorName(),
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
	if insp.Manifest.TenantSpec == nil {
		s.fail(w, r, http.StatusUnprocessableEntity, "the bundle's manifest carries no tenant settings, so no tenant can be made from it; nothing was changed")
		return
	}

	// An import of this tenant that has not finished: this request goes on
	// with it, if it is of the same bundle.
	pending, unfinished, err := s.cfg.Repo.PendingImport(ctx, name)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if unfinished {
		s.resumeImport(w, r, c, pending, req)
		return
	}

	// A tenant of the name, before anything is fetched or committed for it.
	if _, err := s.cfg.Repo.IsPlatformTenant(ctx, name); err == nil {
		s.fail(w, r, http.StatusConflict, "a tenant of that name already exists; restore into it instead, or import under another name")
		return
	} else if !errors.Is(err, gitops.ErrTenantNotFound) {
		s.repoError(w, r, err)
		return
	}

	// The apps' definitions, before anything is committed. A tenant whose
	// manifest names a profile the cluster does not have is never
	// provisioned, and an import that waits for it can only time out.
	missing, err := s.importProfiles(ctx, name, insp.Manifest.TenantSpec, c.meta)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if len(missing) > 0 {
		s.fail(w, r, http.StatusUnprocessableEntity, "this cluster cannot obtain the definition of every app the bundle's tenant has: "+
			strings.Join(missing, "; ")+". Nothing was changed. Add a catalogue that serves them (kubectl gentian catalogues add), "+
			"or install the apps' definitions here, and import again")
		return
	}

	now := time.Now().UTC()
	pending = gitops.PendingImport{
		Tenant: name, Source: insp.Manifest.Tenant, Export: insp.Manifest.Export, Bundle: req.Bundle,
		Restore:     "import-" + now.Format("20060102-150405"),
		RequestedAt: now.Format(time.RFC3339), RequestedBy: c.meta.ActorName(),
	}
	origin := fmt.Sprintf("export %s of %s, of tenant %s", insp.Manifest.Export, insp.Manifest.CreatedAt, insp.Manifest.Tenant)
	res, err := s.cfg.Repo.DeclareTenant(ctx, gitops.ImportedTenant{
		Name: name, Spec: insp.Manifest.TenantSpec, Origin: origin, Pending: pending,
	}, c.meta)
	switch {
	case errors.Is(err, gitops.ErrTenantExists):
		s.fail(w, r, http.StatusConflict, "a tenant of that name already exists; restore into it instead, or import under another name")
		return
	case errors.Is(err, gitops.ErrSingleTenancy), errors.Is(err, backup.ErrNamesTaken):
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.repoError(w, r, err)
		return
	}
	st := ImportStatus{Tenant: name, Phase: "declared", Commit: res.Commit, Bundle: req.Bundle, Restore: pending.Restore,
		Message: "the tenant is committed; the operator provisions it next"}
	if !s.watchImport(pending, req.Decryption, c.meta.ActorName(), &st) {
		s.fail(w, r, http.StatusConflict, "an import of this tenant is already under way")
		return
	}
	s.json(w, http.StatusAccepted, st)
}

// resumeImport goes on with an import that was begun and has not finished.
func (s *Server) resumeImport(w http.ResponseWriter, r *http.Request, c call, pending gitops.PendingImport, req importRequest) {
	if pending.Bundle.Bucket != req.Bundle.Bucket || pending.Bundle.Prefix != req.Bundle.Prefix {
		s.fail(w, r, http.StatusConflict, fmt.Sprintf(
			"tenant %s is being imported from another bundle (%s/%s) and that import has not finished; "+
				"give that bundle and its key to go on with it, or retire the tenant and import again",
			pending.Tenant, pending.Bundle.Bucket, pending.Bundle.Prefix))
		return
	}
	st := ImportStatus{Tenant: pending.Tenant, Phase: "provisioning", Bundle: pending.Bundle, Restore: pending.Restore,
		Message: "going on with the import of this tenant"}
	if !s.watchImport(pending, req.Decryption, c.meta.ActorName(), &st) {
		s.fail(w, r, http.StatusConflict, "an import of this tenant is already under way")
		return
	}
	s.json(w, http.StatusAccepted, st)
}

// watchImport starts the one watcher an import has, and answers false when
// it has one already. st is what the status route says until the watcher
// says more.
func (s *Server) watchImport(pending gitops.PendingImport, key json.RawMessage, actor string, st *ImportStatus) bool {
	if _, busy := s.importing.LoadOrStore(pending.Tenant, struct{}{}); busy {
		return false
	}
	s.imports.Store(pending.Tenant, *st)
	go func() {
		defer s.importing.Delete(pending.Tenant)
		s.finishImport(pending, key, actor)
	}()
	return true
}

// ResumeImports picks up every import a previous run began and did not
// finish. The record committed with the tenant is what says there is one, so
// a restart during an import no longer leaves a tenant that is made and
// never filled.
//
// It has no key: the key a bundle is opened with came with the request and
// was never written down. An import whose restore was already started needs
// none and is watched to its end. One that was still waiting for the tenant
// says so (awaiting-key) until it is asked for again, with the key.
func (s *Server) ResumeImports(ctx context.Context) {
	if s.cfg.Lifecycle == nil {
		return
	}
	pending, err := s.cfg.Repo.PendingImports(ctx)
	if err != nil {
		s.cfg.Log.Warn("pending imports could not be read", "error", err.Error())
		return
	}
	for _, p := range pending {
		s.cfg.Log.Info("resuming the import of a tenant", "tenant", p.Tenant, "restore", p.Restore)
		st := ImportStatus{Tenant: p.Tenant, Phase: "provisioning", Bundle: p.Bundle, Restore: p.Restore,
			Message: "the director restarted during this import and is going on with it"}
		s.watchImport(p, nil, "gentian-director", &st)
	}
}

func (s *Server) importStatus(w http.ResponseWriter, r *http.Request, _ call) {
	tenant := r.PathValue("t")
	if st, ok := s.imports.Load(tenant); ok {
		s.json(w, http.StatusOK, st)
		return
	}
	// Not watched here, and on record: another start of this director began
	// it. Say so, and go on with it.
	pending, unfinished, err := s.cfg.Repo.PendingImport(r.Context(), tenant)
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if !unfinished {
		s.fail(w, r, http.StatusNotFound, "no import of this tenant is under way")
		return
	}
	st := ImportStatus{Tenant: tenant, Phase: "provisioning", Bundle: pending.Bundle, Restore: pending.Restore,
		Message: "this import was begun before this director started; it is going on with it"}
	s.watchImport(pending, nil, "gentian-director", &st)
	s.json(w, http.StatusOK, st)
}

func (s *Server) setImport(name string, mutate func(*ImportStatus)) {
	st, _ := s.imports.Load(name)
	cur, _ := st.(ImportStatus)
	mutate(&cur)
	s.imports.Store(name, cur)
}

// awaitingKey is what an import says that cannot go on without the bundle's
// key, which this director does not have.
const awaitingKey = "the tenant is made and empty. The director restarted while this import waited for it, and the bundle's key, " +
	"which is given with the request and never written down, went with it. Ask for the import again, with the same bundle, " +
	"the same name and the key, and it goes on from here"

// finishImport waits for the operator, then starts the restore and watches
// it to its end. key is nil for an import picked up after a restart.
func (s *Server) finishImport(pending gitops.PendingImport, key json.RawMessage, actor string) {
	name := pending.Tenant
	ctx, cancel := context.WithTimeout(context.Background(), importWait)
	defer cancel()
	fail := func(msg string) {
		s.setImport(name, func(st *ImportStatus) { st.Phase = "failed"; st.Message = msg })
		s.cfg.Log.Error("import failed", "tenant", name, "error", msg)
	}
	restorePath := "/v1/tenants/" + url.PathEscape(name) + "/restores/" + url.PathEscape(pending.Restore)

	// Was the restore started already? Its name was decided with the import,
	// so this can be asked, and a second one is never started.
	started := false
	if code, _, err := s.cfg.Lifecycle.Get(ctx, restorePath, nil); err == nil && code == http.StatusOK {
		started = true
	}
	if !started && len(key) == 0 {
		s.setImport(name, func(st *ImportStatus) { st.Phase = "awaiting-key"; st.Message = awaitingKey })
		s.cfg.Log.Warn("an import waits for its bundle's key to be given again", "tenant", name)
		return
	}

	if !started {
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
				fail("the tenant was not provisioned within " + importWait.String() + ", last: " + s.importMessage(name) +
					". The tenant is committed and empty; ask for the import again to go on waiting, or start a restore by hand once it is provisioned")
				return
			case <-time.After(importPoll):
			}
		}
		status, body, err := s.cfg.Lifecycle.Do(ctx, "/v1/tenants/"+url.PathEscape(name)+"/actions/restore", actor,
			map[string]any{"bundle": pending.Bundle, "decryption": key, "name": pending.Restore})
		if err != nil || status/100 != 2 {
			// Unless it is there after all: another replica of this director
			// was asked to go on with the same import and started it first.
			if code, _, getErr := s.cfg.Lifecycle.Get(ctx, restorePath, nil); getErr != nil || code != http.StatusOK {
				fail("the restore could not be started: " + strings.TrimSpace(string(body)))
				return
			}
		}
	}
	s.setImport(name, func(st *ImportStatus) {
		st.Phase = "restoring"
		st.Restore = pending.Restore
		st.Message = "restoring the bundle into the tenant"
	})
	for {
		code, body, err := s.cfg.Lifecycle.Get(ctx, restorePath, nil)
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
					// The import is done: its record goes, and then the status
					// says so. A record that cannot be removed is found again at
					// the next start, which finds the restore finished and
					// removes it then.
					if _, err := s.cfg.Repo.FinishImport(context.Background(), name, gitops.Meta{
						Principal: "gentian-director",
						Decision:  "the import of this tenant has finished",
					}); err != nil {
						s.cfg.Log.Warn("the record of a finished import could not be removed", "tenant", name, "error", err.Error())
					}
					s.setImport(name, func(st *ImportStatus) {
						st.Phase = "ready"
						st.Message = rs.Message
						st.PasswordResetRequired = rs.PasswordResetRequired
						st.Complete, st.NotRestored, st.Notes = rs.Complete, rs.NotRestored, rs.Notes
					})
					s.cfg.Log.Info("tenant imported", "tenant", name, "restore", pending.Restore)
					return
				case "Failed":
					// The record stays: the tenant is not what the bundle holds.
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

func (s *Server) importMessage(name string) string {
	st, _ := s.imports.Load(name)
	cur, _ := st.(ImportStatus)
	return cur.Message
}

// wantedProfile is one definition a tenant's manifest names.
type wantedProfile struct {
	name, digest, catalogue, what string
}

// importProfiles makes sure of the definition of every app and pinned add-on
// a bundle's tenant lists, and answers the ones this cluster cannot obtain,
// each with why. Nothing is committed unless every one can be had.
//
// An install commits an app's definition before the tenant's manifest names
// it (commitEntry). An import wrote the manifest alone: on a cluster that had
// never installed those apps the tenant stayed Degraded on a profile that was
// not there, and the import failed two hours later. So, for each:
//
//   - A build the bundle records (a digest) is fetched from a catalogue this
//     cluster declares -- the one of the name the bundle records first, then
//     every other -- and committed. The bytes have to hash to the digest,
//     whichever catalogue serves them, so it does not matter which does. A
//     profile already here at that build needs no fetch.
//   - An entry with no build recorded has to be on the cluster already.
func (s *Server) importProfiles(ctx context.Context, tenant string, spec *gentianov1alpha1.TenantSpec, meta gitops.Meta) ([]string, error) {
	var wanted []wantedProfile
	for _, app := range spec.Apps {
		name := app.Profile
		if name == "" && app.ProfileRef != nil {
			name = app.ProfileRef.Name
		}
		if name == "" {
			continue
		}
		wanted = append(wanted, wantedProfile{name: name, digest: app.Digest, catalogue: app.Catalogue, what: "app " + name})
		pinned := map[string]bool{}
		for _, pin := range app.AddonPins {
			pinned[pin.Name] = true
			wanted = append(wanted, wantedProfile{name: pin.Name, digest: pin.Digest, catalogue: pin.Catalogue,
				what: "add-on " + pin.Name + " of " + name})
		}
		for _, addon := range app.Addons {
			if !pinned[addon] {
				wanted = append(wanted, wantedProfile{name: addon, what: "add-on " + addon + " of " + name})
			}
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	settings, err := s.cfg.Repo.Catalogue(ctx)
	if err != nil && !errors.Is(err, gitops.ErrNoClusterClaim) {
		return nil, err
	}
	var sources []visibleSource
	var sourceNames []string
	for _, src := range settings.Sources {
		sources = append(sources, visibleSource{
			Source: catalogue.Source{Key: profilebundle.ClusterOrigin(src.Name), Name: src.Name, URL: src.URL},
			scope:  scopeCluster, addedBy: string(gitops.ByCluster),
		})
		sourceNames = append(sourceNames, src.Name)
	}

	type fetched struct {
		source  visibleSource
		profile *catalogue.Profile
		what    string
	}
	var toCommit []fetched
	var missing []string
	for _, w := range wanted {
		if !gitops.ValidName(w.name) {
			missing = append(missing, w.what+" (its name is not one a profile can have)")
			continue
		}
		have, err := s.cfg.Repo.ProfileOnCluster(ctx, w.name)
		if err != nil {
			return nil, err
		}
		here := gitops.PlatformProfile(w.name) || (have.Present && profilebundle.UsableBy(have.Origin, tenant))
		if w.digest == "" {
			if !here {
				missing = append(missing, w.what+" (the bundle records no build of it, and no profile of that name is on this cluster for this tenant)")
			}
			continue
		}
		digest, err := catalogue.CanonicalDigest(w.digest)
		if err != nil {
			missing = append(missing, w.what+" (the bundle records a digest that is not one: "+w.digest+")")
			continue
		}
		if here && have.Digest == digest {
			continue
		}
		if s.cfg.Catalogue == nil || len(sources) == 0 {
			missing = append(missing, fmt.Sprintf("%s at %s (this cluster has no catalogue to fetch it from)", w.what, profilebundle.Short(digest)))
			continue
		}
		// The catalogue the bundle names first, when this cluster has one of
		// that name.
		ordered := make([]visibleSource, 0, len(sources))
		for _, src := range sources {
			if src.Name == w.catalogue {
				ordered = append(ordered, src)
			}
		}
		for _, src := range sources {
			if src.Name != w.catalogue {
				ordered = append(ordered, src)
			}
		}
		var got *fetched
		for _, src := range ordered {
			profile, err := s.cfg.Catalogue.Fetch(ctx, src.Source, w.name, digest)
			if err != nil {
				s.cfg.Log.InfoContext(ctx, "a catalogue does not serve a build an import needs",
					"request_id", reqID(ctx), "catalogue", src.Name, "profile", w.name, "digest", digest, "error", err.Error())
				continue
			}
			got = &fetched{source: src, profile: profile, what: w.what}
			break
		}
		if got == nil {
			missing = append(missing, fmt.Sprintf("%s at %s (none of this cluster's catalogues serves that build: %s)",
				w.what, profilebundle.Short(digest), strings.Join(sourceNames, ", ")))
			continue
		}
		toCommit = append(toCommit, *got)
	}
	if len(missing) > 0 {
		return missing, nil
	}
	for _, f := range toCommit {
		res, err := s.cfg.Repo.MaterialiseProfile(ctx, f.profile.Name, f.profile.Digest, f.profile.Body, f.source.origin(tenant), meta)
		var taken *gitops.ErrProfileNameTaken
		switch {
		case errors.As(err, &taken):
			return []string{f.what + " (the name is taken on this cluster by a profile of another origin, so this build cannot be put under it)"}, nil
		case errors.Is(err, gitops.ErrBundleTooLarge), errors.Is(err, gitops.ErrProfileStatesOrigin), errors.Is(err, profilebundle.ErrRefused):
			return []string{f.what + " (its bundle is not one this cluster installs: " + err.Error() + ")"}, nil
		case err != nil:
			return nil, err
		}
		if res.Changed {
			s.cfg.Log.InfoContext(ctx, "materialised a catalogue entry for an import",
				"request_id", reqID(ctx), "profile", f.profile.Name, "origin", f.source.origin(tenant), "commit", res.Commit)
		}
	}
	return nil, nil
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
