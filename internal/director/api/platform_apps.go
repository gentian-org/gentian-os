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
	"errors"
	"net/http"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// The apps the platform places itself. A cluster catalogue's index marks an
// entry placedWith: <capability>, and while the cluster has that capability
// every tenant has the app, for everyone; when it no longer has it, no
// tenant does. The entry is in the tenant's manifest like any app, so the
// operator handles it like any app. What differs is who writes it: nobody
// installs or uninstalls it by name, and the App Store does not offer it
// (the catalogue lists it listed: false).
//
// The one capability is llm, the cluster serving models (spec.llm.enabled).
// An entry naming another is left alone.
//
// Only the cluster's catalogs place: a tenant's own catalogue writing an
// app into every other tenant would be a way around every install check.

const (
	// platformAppsEvery is how often the placement is checked. Also run at
	// start, and right after a change to the model settings or a new tenant.
	platformAppsEvery = 5 * time.Minute
	// platformAppsPrincipal is who the commits name.
	platformAppsPrincipal = "platform:capabilities"
	// capabilityLLM is the cluster serving models.
	capabilityLLM = "llm"
	// requiresCapabilityAnnotation names the capability a profile the
	// platform places comes with; the catalogue's index repeats it as
	// placedWith.
	requiresCapabilityAnnotation = "gentianos.io/requires-capability"
)

// platformAppsRun is whether this director places apps at all: like the
// platform's mover, only with a catalogue to read and an operator beside it,
// and not where the mover is switched off (a test drives it by hand).
func (s *Server) platformAppsRun() bool {
	return s.cfg.Catalogue != nil && s.cfg.Lifecycle != nil && !s.cfg.UpdaterOff
}

// startPlatformApps runs the placement beside the requests.
func (s *Server) startPlatformApps() {
	if !s.platformAppsRun() {
		return
	}
	s.watch(func(ctx context.Context) {
		s.placePlatformApps(ctx)
		t := time.NewTicker(platformAppsEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.placePlatformApps(ctx)
			}
		}
	})
}

// placePlatformAppsNow runs one placement beside the request that changed
// what it depends on, so nobody waits for the ticker.
func (s *Server) placePlatformAppsNow() {
	if !s.platformAppsRun() {
		return
	}
	s.watch(func(ctx context.Context) { s.placePlatformApps(ctx) })
}

// PlacePlatformApps is one placement, for a test or a tool that does not
// want to wait for the ticker: how many apps it placed and removed.
func (s *Server) PlacePlatformApps(ctx context.Context) (placed, removed int) {
	return s.placePlatformApps(ctx)
}

// placePlatformApps is one placement. Passes do not overlap: one asked for
// while another runs is skipped, and the next tick does what it would have.
func (s *Server) placePlatformApps(ctx context.Context) (placed, removed int) {
	if !s.platformApps.TryLock() {
		return 0, 0
	}
	defer s.platformApps.Unlock()
	log := s.cfg.Log.With("worker", "platform-apps")

	models, err := s.cfg.Repo.ClusterModels(ctx)
	if err != nil {
		if !errors.Is(err, gitops.ErrNoClusterClaim) {
			log.WarnContext(ctx, "the model settings could not be read; no app is placed or removed", "error", err.Error())
		}
		return 0, 0
	}
	have := map[string]bool{capabilityLLM: models.Enabled}

	cluster, err := s.cfg.Repo.Catalogue(ctx)
	if err != nil {
		log.WarnContext(ctx, "the cluster's catalogs could not be read; no app is placed or removed", "error", err.Error())
		return 0, 0
	}
	type placedApp struct {
		catalogue, name, digest string
		wanted                  bool
	}
	var apps []placedApp
	for _, src := range cluster.Sources {
		listing, err := s.cfg.Catalogue.Index(ctx, clusterSource(src).Source)
		if err != nil {
			// Unread is not "no longer offered": nothing it places is removed.
			log.WarnContext(ctx, "a catalog could not be read; what it places is left as it is",
				"catalogue", src.Name, "error", err.Error())
			continue
		}
		for _, e := range listing.Placed {
			wanted, known := have[e.PlacedWith]
			if !known || e.Digest == "" {
				continue
			}
			apps = append(apps, placedApp{catalogue: src.Name, name: e.Name, digest: e.Digest, wanted: wanted})
		}
	}
	if len(apps) == 0 {
		return 0, 0
	}

	tenants, err := s.cfg.Repo.TenantDetails(ctx)
	if err != nil {
		log.WarnContext(ctx, "the tenants could not be read; no app is placed or removed", "error", err.Error())
		return 0, 0
	}
	everyone := true
	c := call{meta: gitops.Meta{Principal: platformAppsPrincipal, Decision: "the cluster's capabilities"}}
	for _, t := range tenants {
		if t.Purging {
			continue
		}
		if platform, err := s.cfg.Repo.IsPlatformTenant(ctx, t.Name); err != nil || platform {
			continue
		}
		installed := map[string]bool{}
		for _, a := range t.Apps {
			installed[a] = true
		}
		for _, a := range apps {
			switch {
			case a.wanted && !installed[a.name]:
				res, detail, err := s.moveTo(ctx, c, t.Name, a.name, a.catalogue, a.digest, &everyone)
				if err != nil {
					log.WarnContext(ctx, "the platform could not place an app", "tenant", t.Name, "app", a.name, "detail", detail)
					continue
				}
				if res.Changed {
					placed++
					log.InfoContext(ctx, "placed an app", "tenant", t.Name, "app", a.name, "commit", res.Commit)
				}
			case !a.wanted && installed[a.name]:
				res, err := s.cfg.Repo.Uninstall(ctx, t.Name, a.name, c.meta)
				if err != nil {
					log.WarnContext(ctx, "the platform could not remove an app", "tenant", t.Name, "app", a.name, "error", err.Error())
					continue
				}
				if res.Changed {
					removed++
					log.InfoContext(ctx, "removed an app", "tenant", t.Name, "app", a.name, "commit", res.Commit)
				}
			}
		}
	}
	return placed, removed
}

// refusedAsPlatformApp refuses removing by hand an app the platform places,
// and reports that it did.
func (s *Server) refusedAsPlatformApp(w http.ResponseWriter, r *http.Request, profile string) bool {
	definition, err := s.cfg.Repo.ProfileDefinition(r.Context(), profile)
	if err != nil || definition == nil || definition.Annotations[requiresCapabilityAnnotation] == "" {
		return false
	}
	s.fail(w, r, http.StatusConflict, profile+
		" comes with the cluster and is not uninstalled here; it goes when the cluster stops serving models")
	return true
}
