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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/placement"
)

// The apps the platform places itself. A placeable profile (ComponentProfile
// spec.placement) is written into the tenants it names as an entry of
// spec.apps marked placed, so the operator, the contracts, the grants and
// the network paths handle it like any app. What differs is who writes it:
// this worker places it and takes it away, and nobody installs, uninstalls,
// moves or changes it by hand (gitops.ErrPlacedApp).
//
// Two kinds of placement, found in two places:
//
//   - The profiles on the cluster -- the platform's own components, and those
//     a cluster's catalog materialized -- go where their placement says,
//     every tenant, the platform tenant, or user tenants while the cluster
//     offers a store. Which ones go where is the operator's to answer
//     (/v1/placements): it reads the cluster. The entry names the profile
//     alone and runs the build the cluster has.
//   - A catalog entry placed while the cluster has a capability is found in
//     the cluster catalogs' indexes (placedWith), before it is on the
//     cluster, and placed at the build the index lists, for everyone. The
//     one capability is llm, the cluster serving models (spec.llm.enabled).
//     Only the cluster's catalogs place: a tenant's own catalog writing an
//     app into every other tenant would be a way around every install check.
//
// And the contracts the platform pre-approves between components it places
// itself (placement.Grant) are granted in every tenant that has both, as the
// platform's own grant; a contract the tenant's grant already names is left
// as the administrator wrote it.

const (
	// platformAppsEvery is how often the placement is checked. Also run at
	// start, and right after a change to the model settings, a new tenant or
	// an import.
	platformAppsEvery = 5 * time.Minute
	// platformAppsPrincipal is who the commits name.
	platformAppsPrincipal = "platform:placement"
	// capabilityLLM is the cluster serving models.
	capabilityLLM = "llm"
)

// platformAppsRun is whether this director places apps at all: only with an
// operator beside it to say what goes where, and not where the mover is
// switched off (a test drives it by hand).
func (s *Server) platformAppsRun() bool {
	return s.cfg.Lifecycle != nil && !s.cfg.UpdaterOff
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

// clusterPlacements asks the operator which of the cluster's placeable
// profiles go to which tenants.
func (s *Server) clusterPlacements(ctx context.Context) (placement.Answer, error) {
	if s.cfg.Lifecycle == nil {
		return placement.Answer{}, errors.New("no operator to ask")
	}
	status, body, err := s.cfg.Lifecycle.Get(ctx, "/v1/placements", nil)
	if err != nil {
		return placement.Answer{}, err
	}
	if status != http.StatusOK {
		return placement.Answer{}, fmt.Errorf("the operator answered %d: %s", status, lifecycle.ErrorMessage(body))
	}
	var answer placement.Answer
	if err := json.Unmarshal(body, &answer); err != nil {
		return placement.Answer{}, fmt.Errorf("the operator's placement does not parse: %w", err)
	}
	return answer, nil
}

// capabilityApp is a catalog entry placed with a capability of the cluster.
type capabilityApp struct {
	catalogue, name, digest string
	wanted                  bool
}

// capabilityApps are the entries the cluster catalogs' indexes place with a
// capability, each with whether the cluster has the capability now. An
// entry of a catalog that could not be read is not among them: unread is
// not "no longer offered", and nothing it places is removed.
func (s *Server) capabilityApps(ctx context.Context) (apps []capabilityApp) {
	log := s.cfg.Log.With("worker", "platform-apps")
	if s.cfg.Catalogue == nil {
		return nil
	}
	models, err := s.cfg.Repo.ClusterModels(ctx)
	if err != nil {
		if !errors.Is(err, gitops.ErrNoClusterClaim) {
			log.WarnContext(ctx, "the model settings could not be read; no app placed with them is placed or removed", "error", err.Error())
		}
		return nil
	}
	have := map[string]bool{capabilityLLM: models.Enabled}
	cluster, err := s.cfg.Repo.Catalogue(ctx)
	if err != nil {
		log.WarnContext(ctx, "the cluster's catalogs could not be read; no app placed with a capability is placed or removed", "error", err.Error())
		return nil
	}
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
			apps = append(apps, capabilityApp{catalogue: src.Name, name: e.Name, digest: e.Digest, wanted: wanted})
		}
	}
	return apps
}

// placePlatformApps is one placement. Passes do not overlap: one asked for
// while another runs is skipped, and the next tick does what it would have.
func (s *Server) placePlatformApps(ctx context.Context) (placed, removed int) {
	if !s.platformApps.TryLock() {
		return 0, 0
	}
	defer s.platformApps.Unlock()
	log := s.cfg.Log.With("worker", "platform-apps")

	answer, err := s.clusterPlacements(ctx)
	answered := err == nil
	if !answered {
		log.WarnContext(ctx, "the operator could not say what the platform places; none of the cluster's placeable profiles is placed or removed",
			"error", err.Error())
	}
	capability := s.capabilityApps(ctx)
	if !answered && len(capability) == 0 {
		return 0, 0
	}

	tenants, err := s.cfg.Repo.TenantDetails(ctx)
	if err != nil {
		log.WarnContext(ctx, "the tenants could not be read; no app is placed or removed", "error", err.Error())
		return 0, 0
	}
	// What this pass found withdrawn, by tenant and app. An entry is taken
	// out only when the pass before found it withdrawn too: one answer
	// that briefly lacks a profile -- an Application resyncing, a profile
	// being replaced -- does not remove it from every tenant at once.
	absent := map[string]bool{}
	c := call{meta: gitops.Meta{Principal: platformAppsPrincipal, Decision: "the platform's placement"}}
	for _, t := range tenants {
		if t.Purging {
			continue
		}
		platform, err := s.cfg.Repo.IsPlatformTenant(ctx, t.Name)
		if err != nil {
			log.WarnContext(ctx, "whether a tenant is the platform tenant could not be read; nothing is placed in it",
				"tenant", t.Name, "error", err.Error())
			continue
		}
		apps, err := s.cfg.Repo.Apps(ctx, t.Name)
		if err != nil {
			log.WarnContext(ctx, "a tenant's apps could not be read; nothing is placed in it", "tenant", t.Name, "error", err.Error())
			continue
		}
		have := map[string]gitops.App{}
		for _, a := range apps {
			have[a.Profile] = a
		}
		if answered {
			set := answer.For(platform)
			// What the platform has placed in this tenant once this pass
			// is through, for the contracts it pre-approves between them.
			ours := map[string]bool{}
			for _, a := range apps {
				if a.Placed {
					ours[a.Profile] = true
				}
			}
			for _, name := range set.Wanted {
				// Placed already, or an entry of the name the tenant
				// installed itself: that one is the tenant's and stays
				// as it is.
				if _, ok := have[name]; ok {
					continue
				}
				res, err := s.cfg.Repo.Place(ctx, t.Name, name, "", "", false, c.meta)
				if err != nil {
					log.WarnContext(ctx, "the platform could not place an app", "tenant", t.Name, "app", name, "error", err.Error())
					continue
				}
				ours[name] = true
				if res.Changed {
					placed++
					log.InfoContext(ctx, "placed an app", "tenant", t.Name, "app", name, "commit", res.Commit)
				}
			}
			for _, a := range apps {
				if !a.Placed || set.Has(a.Profile) || !set.Decided(a.Profile) {
					continue
				}
				key := t.Name + "/" + a.Profile
				absent[key] = true
				if !s.placementAbsent[key] {
					continue
				}
				res, err := s.cfg.Repo.Unplace(ctx, t.Name, a.Profile, c.meta)
				if err != nil {
					log.WarnContext(ctx, "the platform could not remove an app", "tenant", t.Name, "app", a.Profile, "error", err.Error())
					continue
				}
				delete(ours, a.Profile)
				if res.Changed {
					removed++
					log.InfoContext(ctx, "removed an app", "tenant", t.Name, "app", a.Profile, "commit", res.Commit)
				}
			}
			for _, g := range answer.Grants {
				if !ours[g.App] || !ours[g.Provider] {
					continue
				}
				res, err := s.cfg.Repo.PreApproveConsume(ctx, t.Name, g.App, g.Contract, g.Capabilities, c.meta)
				if err != nil {
					log.WarnContext(ctx, "the platform could not pre-approve a contract", "tenant", t.Name, "app", g.App,
						"contract", g.Contract, "error", err.Error())
					continue
				}
				if res.Changed {
					log.InfoContext(ctx, "pre-approved a contract", "tenant", t.Name, "app", g.App, "contract", g.Contract, "commit", res.Commit)
				}
			}
		}
		// A capability's apps go to user tenants only: nobody uses an app
		// in the platform tenant. Their entries are the tenant's apps as
		// they always were, unmarked: installed for everyone at the build
		// the catalog lists when the cluster gains the capability, and
		// uninstalled when it loses it.
		if platform {
			continue
		}
		for _, a := range capability {
			_, present := have[a.name]
			switch {
			case a.wanted && !present:
				everyone := true
				res, detail, err := s.moveTo(ctx, c, t.Name, a.name, a.catalogue, a.digest, &everyone)
				if err != nil {
					log.WarnContext(ctx, "the platform could not place an app", "tenant", t.Name, "app", a.name, "detail", detail)
					continue
				}
				if res.Changed {
					placed++
					log.InfoContext(ctx, "placed an app", "tenant", t.Name, "app", a.name, "commit", res.Commit)
				}
			case !a.wanted && present:
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
	if answered {
		s.placementAbsent = absent
	}
	return placed, removed
}

// refusedAsCapabilityApp refuses removing by hand an app the platform
// places while the cluster has a capability, and reports that it did.
func (s *Server) refusedAsCapabilityApp(w http.ResponseWriter, r *http.Request, profile string) bool {
	definition, err := s.cfg.Repo.ProfileDefinition(r.Context(), profile)
	if err != nil || definition == nil {
		return false
	}
	if p := definition.PlacementOf(); p == nil || p.While != gentianov1alpha1.PlacementWhileLLM {
		return false
	}
	s.fail(w, r, http.StatusConflict, profile+
		" comes with the cluster and is not uninstalled here; it goes when the cluster stops serving models")
	return true
}

// placedForNewTenant are the components the platform places in a new user
// tenant, for the commit that creates it. A cluster whose operator cannot
// say yet creates the tenant without them, and the next placement adds them.
func (s *Server) placedForNewTenant(ctx context.Context) []string {
	if s.cfg.Lifecycle == nil {
		return nil
	}
	answer, err := s.clusterPlacements(ctx)
	if err != nil {
		s.cfg.Log.WarnContext(ctx, "the operator could not say what the platform places; the tenant is created without it "+
			"and the next placement adds it", "error", err.Error())
		return nil
	}
	return answer.Users.Wanted
}
