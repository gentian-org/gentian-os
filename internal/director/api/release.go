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
	"time"

	"github.com/gentian-org/gentian-os/internal/applifecycle"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// The platform release a cluster runs (docs/concepts/versioning.md §2). The
// claim declares it, the operator applies it and reports what it did and
// what the release index offers; the director is the one writer, for a
// person who sets a version and for the auto-upgrade mover alike, and the
// commit it writes moves the catalog source with the release.

// releaseMoverPrincipal is who the auto-upgrade commits name.
const releaseMoverPrincipal = "policy:autoUpgrade"

// releaseView is the answer to GET /v1/clusters/{c}/release.
type releaseView struct {
	Cluster string `json:"cluster"`
	// Spec is what the claim in git declares.
	Spec gitops.ReleaseSpec `json:"spec"`
	// Status and Platform are the operator's: status.release and
	// status.platform of the claim, as the cluster has them. Nil with
	// Partial set when the cluster could not be asked.
	Status   map[string]any `json:"status,omitempty"`
	Platform map[string]any `json:"platform,omitempty"`
	Partial  string         `json:"partial,omitempty"`
	// Window is the maintenance window auto-upgrade moves inside
	// (spec.catalogue.updates.window), and whether it is open now.
	Window     *gitops.UpdateWindow `json:"window,omitempty"`
	WindowOpen bool                 `json:"windowOpen"`
}

func (s *Server) clusterRelease(w http.ResponseWriter, r *http.Request, _ call) {
	view, err := s.releaseView(r.Context())
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	s.json(w, http.StatusOK, view)
}

func (s *Server) releaseView(ctx context.Context) (*releaseView, error) {
	spec, err := s.cfg.Repo.Release(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := s.cfg.Repo.UpdatePolicy(ctx)
	if err != nil {
		return nil, err
	}
	view := &releaseView{Cluster: s.cfg.Cluster, Spec: spec, Window: policy.Window, WindowOpen: policy.Window.Open(time.Now())}
	if s.cfg.Lifecycle == nil {
		view.Partial = "the cluster was not asked: the director has no operator to ask"
		return view, nil
	}
	status, body, err := s.cfg.Lifecycle.Get(ctx, applifecycle.ReleasePath, nil)
	if err != nil || status != http.StatusOK {
		view.Partial = "the cluster's release could not be read: " + upstreamFailure(status, body, err)
		return view, nil
	}
	var answer applifecycle.ReleaseView
	if err := json.Unmarshal(body, &answer); err != nil {
		view.Partial = "the cluster's release could not be read: " + err.Error()
		return view, nil
	}
	view.Status, view.Platform = answer.Status, answer.Platform
	return view, nil
}

// setRelease is POST /v1/clusters/{c}/actions/set-release: {"version":
// "v0.5.1", "autoUpgrade": "patch"} or {"track": "develop"}. A version
// without a mode keeps the mode the claim has.
func (s *Server) setRelease(w http.ResponseWriter, r *http.Request, c call) {
	var want gitops.ReleaseSpec
	if err := decode(r, &want); err != nil || !want.Declared() {
		s.fail(w, r, http.StatusBadRequest, `body must be {"version": "vX.Y.Z", "autoUpgrade": "none|patch|minor"} or {"track": "<branch>"}`)
		return
	}
	if want.Version != "" && want.AutoUpgrade == "" {
		if current, err := s.cfg.Repo.Release(r.Context()); err == nil && current.Version != "" {
			want.AutoUpgrade = current.AutoUpgrade
		}
	}
	res, err := s.cfg.Repo.SetRelease(r.Context(), want, c.meta)
	if errors.Is(err, gitops.ErrInvalidRelease) {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// RunReleaseMove is one pass of the auto-upgrade mover, for a test: whether
// it moved the cluster.
func (s *Server) RunReleaseMove(ctx context.Context) bool { return s.runReleaseMove(ctx) }

// runReleaseMove moves the cluster to the release its autoUpgrade mode
// takes, inside the window: the operator has compared the index with the
// claim and written the answer on status.release.available, so what is
// taken here is what a person reads there. One signed commit, as
// policy:autoUpgrade; the operator does the rest.
func (s *Server) runReleaseMove(ctx context.Context) bool {
	if s.cfg.Lifecycle == nil {
		return false
	}
	log := s.cfg.Log.With("worker", "release")
	view, err := s.releaseView(ctx)
	if err != nil {
		log.WarnContext(ctx, "the cluster's release could not be read; nothing is moved", "error", err.Error())
		return false
	}
	if view.Spec.Version == "" || view.Spec.AutoUpgrade == "" || view.Spec.AutoUpgrade == "none" {
		return false
	}
	if view.Partial != "" {
		log.WarnContext(ctx, "the cluster's side of the release could not be read; nothing is moved", "detail", view.Partial)
		return false
	}
	if pending, _ := view.Status["pending"].(string); pending != "" {
		s.announcePendingRelease(ctx, view.Spec.Version, pending, view.Status)
	}
	available, _ := view.Status["available"].(string)
	if available == "" || available == view.Spec.Version {
		return false
	}
	if !view.WindowOpen {
		log.InfoContext(ctx, "a release waits for the maintenance window", "available", available, "running", view.Spec.Version)
		return false
	}
	want := gitops.ReleaseSpec{Version: available, AutoUpgrade: view.Spec.AutoUpgrade}
	meta := gitops.Meta{Principal: releaseMoverPrincipal, Decision: "policy autoUpgrade " + view.Spec.AutoUpgrade + " inside the window"}
	res, err := s.cfg.Repo.SetRelease(ctx, want, meta)
	if err != nil {
		log.WarnContext(ctx, "the release could not be written", "release", available, "error", err.Error())
		return false
	}
	if res.Changed {
		log.InfoContext(ctx, "the cluster moves to a release by its autoUpgrade mode", "from", view.Spec.Version, "to", available, "mode", view.Spec.AutoUpgrade, "commit", res.Commit)
		s.notifyAdministrators(ctx,
			fmt.Sprintf("Gentian OS: cluster %s moves to %s", s.cfg.Cluster, available),
			fmt.Sprintf("The cluster %s was on %s and moves to %s now, by its autoUpgrade mode %s inside the maintenance window.\n\nThe operator applies the release; follow it with:\n  kubectl gentian release show\n", s.cfg.Cluster, view.Spec.Version, available, view.Spec.AutoUpgrade),
			"", "info")
	}
	return res.Changed
}

// announcePendingRelease tells the administrators once that a release
// waits for them: the first pass that sees it, and again only when the
// pending release changes. The memory is the director's own; a restarted
// director says it once more, which is the right side to err on.
func (s *Server) announcePendingRelease(ctx context.Context, running, pending string, status map[string]any) {
	s.releaseNotices.Lock()
	seen := s.releaseNotices.pending == pending
	if !seen {
		s.releaseNotices.pending = pending
	}
	s.releaseNotices.Unlock()
	if seen {
		return
	}
	notes, _ := status["pendingNotes"].(string)
	text := fmt.Sprintf("Release %s of Gentian OS is out. The cluster %s runs %s, and its autoUpgrade mode does not take %s by itself.\n\nTo move the cluster:\n  kubectl gentian release set %s\nor take it in the administration console, under Release.\n", pending, s.cfg.Cluster, running, pending, pending)
	if notes != "" {
		text += "\nRelease notes: " + notes + "\n"
	}
	s.notifyAdministrators(ctx, fmt.Sprintf("Gentian OS %s waits for cluster %s", pending, s.cfg.Cluster), text, notes, "warning")
}

// notifyAdministrators says something to the cluster's administrators in
// both places they read: a notice in the platform tenant's console, and a
// mail through the operator, which holds the platform's mail settings. A
// cluster without mail gets the notice alone; neither failure is the
// mover's to stop on.
func (s *Server) notifyAdministrators(ctx context.Context, subject, text, link, severity string) {
	if s.cfg.Lifecycle == nil {
		return
	}
	log := s.cfg.Log.With("worker", "release")
	notice := map[string]any{"title": subject, "body": text, "severity": severity}
	if link != "" {
		notice["linkUrl"] = link
		notice["linkLabel"] = "Release notes"
	}
	if status, body, err := s.cfg.Lifecycle.Do(ctx, "/v1/tenants/"+authz.PlatformTenant+"/actions/notify", releaseMoverPrincipal, notice); err != nil || status >= 300 {
		log.WarnContext(ctx, "the release notice could not be published in the console", "detail", upstreamFailure(status, body, err))
	}
	status, body, err := s.cfg.Lifecycle.Do(ctx, applifecycle.NotifyAdministratorsPath, releaseMoverPrincipal,
		applifecycle.NotifyAdministratorsRequest{Subject: subject, Text: text})
	switch {
	case err == nil && status == http.StatusAccepted:
		log.InfoContext(ctx, "the cluster's administrators were mailed", "subject", subject)
	case err == nil && status == http.StatusServiceUnavailable:
		log.InfoContext(ctx, "no mail to the administrators: the cluster has no mail or no administrator with an address", "detail", upstreamFailure(status, body, err))
	default:
		log.WarnContext(ctx, "the administrators could not be mailed", "detail", upstreamFailure(status, body, err))
	}
}
