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
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// The platform's own mover: what "latest" means (docs/design/versioning.md
// §4.1). Inside the cluster's window it moves apps in latest mode, and their
// pinned add-ons, to the newest build their catalog offers, one tenant
// first, the rest once that one's move has ended well. A move that is rolled back or fails holds every
// other tenant's move to that build: the report says so, and a person
// decides. Nothing is stored between runs; each run reads the report again,
// which is also what a person reads, so the two never disagree.

const (
	// updaterEvery is how often the mover looks.
	updaterEvery = 5 * time.Minute
	// updaterPrincipal is who the commits name.
	updaterPrincipal = "policy:latest"
	// updaterBatch is how many tenants are moved to one build per run once
	// the first has come up: enough to finish a cluster of tens of tenants
	// in a window, few enough that a run is over before the next.
	updaterBatch = 5
)

// startUpdater runs the mover beside the requests, when the director has a
// catalogue to read and an operator to ask. A director-dev or a test
// without either moves nothing.
func (s *Server) startUpdater() {
	if s.cfg.Catalogue == nil || s.cfg.Lifecycle == nil || s.cfg.UpdaterOff {
		return
	}
	s.watch(func(ctx context.Context) {
		t := time.NewTicker(updaterEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runLatestMoves(ctx)
			}
		}
	})
}

// RunLatestMoves is one pass of the mover, for a test or a tool that does
// not want to wait for the ticker: how many apps it moved.
func (s *Server) RunLatestMoves(ctx context.Context) int { return s.runLatestMoves(ctx) }

// runLatestMoves is one pass of the mover: what it moved, and why it left
// the rest, for the log.
func (s *Server) runLatestMoves(ctx context.Context) (moved int) {
	log := s.cfg.Log.With("worker", "updater")
	report, err := s.updateReport(ctx, "")
	if err != nil {
		log.WarnContext(ctx, "the update report could not be made; nothing is moved", "error", err.Error())
		return 0
	}
	if !report.WindowOpen {
		return 0
	}
	if report.Partial != "" {
		// Without the cluster's side a canary cannot be told from a failure.
		log.WarnContext(ctx, "the cluster's side of the report could not be read; nothing is moved", "detail", report.Partial)
		return 0
	}
	// Per build: which tenants are behind it, and how far the others got.
	type target struct {
		catalogue string
		behind    []updateEntry
		underWay  int
		failed    int
		done      int
	}
	targets := map[string]*target{}
	keys := []string{}
	for _, e := range report.Entries {
		if e.Newest == "" || e.Unmet != "" || e.Mode != string(gitops.UpdateLatest) {
			continue
		}
		key := e.Profile + "@" + e.Newest
		tg, ok := targets[key]
		if !ok {
			tg = &target{catalogue: e.Catalogue}
			targets[key] = tg
			keys = append(keys, key)
		}
		switch {
		case e.Pinned == e.Newest && (e.Running == e.Newest || e.AddonOf != ""):
			// An add-on runs inside its app's release: pinned is as far as
			// the report can follow it.
			tg.done++
		case e.Pinned == e.Newest:
			// Asked already and not there yet: a move under way, or one
			// that ended badly.
			if e.Rollout != nil && (e.Rollout.Phase == "RolledBack" || e.Rollout.Phase == "Failed") {
				tg.failed++
			} else {
				tg.underWay++
			}
		case e.Outdated && len(e.Held) == 0:
			tg.behind = append(tg.behind, e)
		}
	}
	meta := gitops.Meta{Principal: updaterPrincipal, Decision: "policy latest inside the window"}
	c := call{meta: meta}
	for _, key := range keys {
		tg := targets[key]
		switch {
		case len(tg.behind) == 0:
			continue
		case tg.failed > 0:
			log.InfoContext(ctx, "a move to this build was rolled back or failed; the other tenants are left where they are",
				"build", key, "behind", len(tg.behind))
			continue
		case tg.underWay > 0:
			// The canary, or a batch, is still rolling: wait for it.
			continue
		}
		batch := 1
		if tg.done > 0 {
			batch = updaterBatch
		}
		for i := 0; i < batch && i < len(tg.behind); i++ {
			e := tg.behind[i]
			e.Catalogue = tg.catalogue
			res, detail, err := s.moveEntry(ctx, c, e)
			if err != nil {
				log.WarnContext(ctx, "the platform could not move an app", "tenant", e.Tenant, "app", e.Profile, "to", e.Newest, "detail", detail)
				continue
			}
			moved++
			log.InfoContext(ctx, "the platform moved an app to the newest build", "tenant", e.Tenant, "app", e.Profile,
				"addonOf", e.AddonOf, "to", e.Newest, "status", res.Status, "commit", res.Commit, "canary", tg.done == 0)
		}
	}
	return moved
}
