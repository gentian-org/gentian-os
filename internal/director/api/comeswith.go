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
	"fmt"
	"net/http"
	"slices"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// The apps an app comes with (ComponentProfile spec.comesWith). Installing
// the app installs each one the tenant does not have, by the path a move
// takes: found in a catalog the tenant sees, fetched, checked against its
// digest, held to the floor and committed with its profile. Uninstalling the
// app removes the ones that came with it. One level: what an app that came
// along comes with itself is not followed.
//
// A move to another build does the same for what that build comes with, and
// a pass beside the platform's mover does it for every installed app, so
// that a tenant already at a build has what the build comes with. The pass
// also uninstalls what came with an app and the build it has arrived at no
// longer names.

// comesWithWait bounds the writes of an install that brings other apps: the
// profile of each, their entries and the app's own. They are made whether or
// not the caller is still waiting.
const comesWithWait = 2 * time.Minute

// companion is one app to install with another: where its build is, or no
// catalog where the cluster already holds its profile for this tenant.
type companion struct {
	name, catalogue, digest string
}

// errComesWithRefused is a move held back by an app the build comes with
// that cannot be had.
var errComesWithRefused = errors.New("an app it comes with cannot be installed")

// installComesWith installs what definition comes with and the tenant does
// not have, each marked as having come with profile, and returns their
// names. preferred is the catalog the app itself is installed from, asked
// first. It answers the refusal itself and reports false; every app is found
// before any is written, so a refusal for one that cannot be found changes
// nothing.
func (s *Server) installComesWith(
	w http.ResponseWriter, r *http.Request, c call, tenant, profile string,
	definition *gentianov1alpha1.ComponentProfile, preferred string, defaultGrant *bool,
) ([]string, bool) {
	came, refusal, err := s.bringComesWith(r.Context(), c, tenant, profile, definition, preferred, defaultGrant)
	if err != nil {
		s.repoError(w, r, err)
		return nil, false
	}
	if refusal != "" {
		s.fail(w, r, http.StatusUnprocessableEntity, refusal)
		return nil, false
	}
	return came, true
}

// bringComesWith is installComesWith without a response writer, for an
// install and for a move alike: the names it installed, or in refusal why
// nothing was.
func (s *Server) bringComesWith(
	ctx context.Context, c call, tenant, profile string,
	definition *gentianov1alpha1.ComponentProfile, preferred string, defaultGrant *bool,
) (came []string, refusal string, err error) {
	if definition == nil || len(definition.Spec.ComesWith) == 0 {
		return nil, "", nil
	}
	apps, err := s.cfg.Repo.Apps(ctx, tenant)
	if err != nil && !errors.Is(err, gitops.ErrTenantNotFound) {
		return nil, "", err
	}
	var plan []companion
	for _, name := range definition.Spec.ComesWith {
		if name == profile || slices.ContainsFunc(apps, func(a gitops.App) bool { return a.Profile == name }) {
			continue
		}
		found, why, err := s.findCompanion(ctx, tenant, name, preferred)
		if err != nil {
			return nil, "", err
		}
		if why != "" {
			return nil, fmt.Sprintf("%s comes with %s, and %s. Nothing was installed.", profile, name, why), nil
		}
		plan = append(plan, found)
	}
	var with []gitops.CameWith
	for _, p := range plan {
		if p.catalogue != "" {
			if _, _, detail, err := s.materialise(ctx, c, "app", tenant, p.name, p.catalogue, p.digest); err != nil {
				return nil, fmt.Sprintf("%s comes with %s, which cannot be installed here: %s", profile, p.name, detail), nil
			}
		}
		with = append(with, gitops.CameWith{Profile: p.name, Digest: p.digest, Catalogue: p.catalogue})
	}
	if len(with) == 0 {
		return nil, "", nil
	}
	_, came, err = s.cfg.Repo.InstallWith(ctx, tenant, profile, with, defaultGrant, c.meta)
	return came, "", err
}

// findCompanion answers where the build of an app that comes with another
// is: the catalog the other app is from first, then the others the tenant
// sees, then the profile the cluster already holds for this tenant. A
// profile the platform ships for the apps that come with it is in no catalog
// and not in the repository: the cluster holds it, for every tenant. why
// says in words that it was found nowhere.
func (s *Server) findCompanion(ctx context.Context, tenant, name, preferred string) (companion, string, error) {
	if gitops.BroughtPlatformProfile(name) {
		return companion{name: name}, "", nil
	}
	sources, _, err := s.visibleSources(ctx, tenant)
	if err != nil {
		return companion{}, "", err
	}
	unread := map[string]bool{}
	if src, entry, ok := s.newestFor(ctx, sources, gitops.App{Profile: name, Catalogue: preferred}, unread); ok && entry.Digest != "" && entry.PlacedWith == "" {
		return companion{name: name, catalogue: src.Name, digest: entry.Digest}, "", nil
	}
	have, err := s.cfg.Repo.ProfileOnCluster(ctx, name)
	if err != nil {
		return companion{}, "", err
	}
	if have.Present && profilebundle.UsableBy(have.Origin, tenant) {
		return companion{name: name}, "", nil
	}
	if len(unread) > 0 {
		return companion{}, "a catalog the tenant sees could not be read to look for it", nil
	}
	return companion{}, "no catalog the tenant sees offers it", nil
}

// heirsOf answers, for each app that came with profile, the other installed
// app that comes with it too, where there is one: the entry then stays and
// is that app's. Nothing is read where nothing came with profile.
func (s *Server) heirsOf(ctx context.Context, tenant, profile string) (map[string]string, error) {
	apps, err := s.cfg.Repo.Apps(ctx, tenant)
	if err != nil {
		if errors.Is(err, gitops.ErrTenantNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var came []string
	for _, a := range apps {
		if a.CameWith == profile {
			came = append(came, a.Profile)
		}
	}
	if len(came) == 0 {
		return nil, nil
	}
	heirs := map[string]string{}
	for _, other := range apps {
		if other.Profile == profile || slices.Contains(came, other.Profile) {
			continue
		}
		definition, err := s.cfg.Repo.ProfileDefinition(ctx, other.Profile)
		if err != nil || definition == nil {
			// A profile that cannot be read names nothing it comes with.
			continue
		}
		for _, name := range came {
			if heirs[name] == "" && slices.Contains(definition.Spec.ComesWith, name) {
				heirs[name] = other.Profile
			}
		}
	}
	return heirs, nil
}

// comesWithPrincipal is who the pass's commits name.
const comesWithPrincipal = "platform:comes-with"

// RunComesWith is one pass over every tenant's apps, for a test or a tool
// that does not want to wait for the ticker: how many apps it installed.
func (s *Server) RunComesWith(ctx context.Context) int { return s.runComesWith(ctx) }

// runComesWith installs, for every app a tenant has, what the build the
// tenant is at comes with and the tenant does not have. The build is read
// from what the cluster holds of it; an app at a build the cluster does not
// hold is left to its next move.
func (s *Server) runComesWith(ctx context.Context) (installed int) {
	log := s.cfg.Log.With("worker", "comes-with")
	tenants, err := s.cfg.Repo.Tenants(ctx)
	if err != nil {
		log.WarnContext(ctx, "the tenants could not be listed; nothing is installed", "error", err.Error())
		return 0
	}
	c := call{meta: gitops.Meta{Principal: comesWithPrincipal, Decision: "the build an app is at comes with it"}}
	rollouts, _, _ := s.clusterSide(ctx)
	for _, tenant := range tenants {
		installed += s.bringToTenant(ctx, c, tenant)
		s.letGo(ctx, c, tenant, rollouts)
	}
	return installed
}

// letGo uninstalls, in one tenant, the apps that came with an app and that
// the build the app is at no longer names. Their data stays, as after any
// uninstall. One that another installed app comes with too stays and is
// marked with that app.
//
// Only once the app has arrived: the operator reports that it runs the build
// it is pinned to. A move under way may still be rolled back to the build
// that names the app, and a cluster that cannot be asked says nothing.
// An app with no pin runs the profile the cluster holds, which is read as it
// is. Nothing is removed where the build's profile cannot be read, or where
// the app an entry came with is not installed.
func (s *Server) letGo(ctx context.Context, c call, tenant string, rollouts map[string]operatorRollout) (removed int) {
	log := s.cfg.Log.With("worker", "comes-with", "tenant", tenant)
	apps, err := s.cfg.Repo.Apps(ctx, tenant)
	if err != nil {
		return 0
	}
	// names[app] are what the build an installed app is at comes with; no
	// entry where that cannot be said yet.
	names := map[string][]string{}
	for _, a := range apps {
		if a.Placed || a.CameWith != "" {
			continue
		}
		if a.Digest != "" {
			if ro, ok := rollouts[tenant+"/"+a.Profile]; !ok || ro.Running != a.Digest {
				continue
			}
		}
		if definition := s.definitionAt(ctx, a); definition != nil {
			names[a.Profile] = append([]string{}, definition.Spec.ComesWith...)
		}
	}
	gone := map[string][]string{}
	for _, a := range apps {
		if a.Placed || a.CameWith == "" {
			continue
		}
		if with, known := names[a.CameWith]; known && !slices.Contains(with, a.Profile) {
			gone[a.CameWith] = append(gone[a.CameWith], a.Profile)
		}
	}
	for with, list := range gone {
		// An heir is an app that has arrived at a build naming the entry.
		heirs := map[string]string{}
		for _, name := range list {
			for _, other := range apps {
				if other.Profile != with && slices.Contains(names[other.Profile], name) {
					heirs[name] = other.Profile
					break
				}
			}
		}
		res, went, err := s.cfg.Repo.LetGo(ctx, tenant, with, list, heirs, c.meta)
		if err != nil {
			log.WarnContext(ctx, "what an app no longer comes with could not be uninstalled", "app", with, "error", err.Error())
			continue
		}
		if res.Changed {
			removed += len(went)
			log.InfoContext(ctx, "uninstalled what an app no longer comes with", "app", with, "apps", went, "commit", res.Commit)
		}
	}
	return removed
}

// bringToTenant is runComesWith for one tenant.
func (s *Server) bringToTenant(ctx context.Context, c call, tenant string) (installed int) {
	log := s.cfg.Log.With("worker", "comes-with", "tenant", tenant)
	apps, err := s.cfg.Repo.Apps(ctx, tenant)
	if err != nil {
		log.WarnContext(ctx, "the tenant's apps could not be read", "error", err.Error())
		return 0
	}
	has := map[string]bool{}
	for _, a := range apps {
		has[a.Profile] = true
	}
	for _, a := range apps {
		if a.Placed || a.CameWith != "" {
			continue
		}
		definition := s.definitionAt(ctx, a)
		if definition == nil || !slices.ContainsFunc(definition.Spec.ComesWith, func(name string) bool { return !has[name] }) {
			continue
		}
		grant := a.DefaultGrant
		came, refusal, err := s.bringComesWith(ctx, c, tenant, a.Profile, definition, a.Catalogue, &grant)
		switch {
		case err != nil:
			log.WarnContext(ctx, "what an app comes with could not be installed", "app", a.Profile, "error", err.Error())
		case refusal != "":
			log.WarnContext(ctx, "what an app comes with could not be installed", "app", a.Profile, "detail", refusal)
		default:
			for _, name := range came {
				has[name] = true
			}
			installed += len(came)
			log.InfoContext(ctx, "installed what an app comes with", "app", a.Profile, "with", came)
		}
	}
	s.approveAmong(ctx, tenant)
	return installed
}

// approveAmong pre-approves, in one tenant, the contracts among an app and
// the apps that came with it: what one of them asks of another, at the build
// each is at. The tenant's administrator said yes to all of them by
// installing the one, and it does nothing without the others. A contract
// with any other app of the tenant -- one the tenant installed itself, one
// the platform places -- still waits for the administrator; so does more of
// a contract the grant already names. It is asked after an install, after a
// move and by the pass, so a contract a later build adds among the same apps
// is pre-approved as the first ones were.
func (s *Server) approveAmong(ctx context.Context, tenant string) (approved int) {
	log := s.cfg.Log.With("worker", "comes-with", "tenant", tenant)
	apps, err := s.cfg.Repo.Apps(ctx, tenant)
	if err != nil {
		if !errors.Is(err, gitops.ErrTenantNotFound) {
			log.WarnContext(ctx, "the tenant's apps could not be read; no contract is pre-approved", "error", err.Error())
		}
		return 0
	}
	// together[app] is the app a came-with app came with, and an app that
	// brought others is its own.
	together := map[string]string{}
	for _, a := range apps {
		if a.CameWith != "" && !a.Placed {
			together[a.Profile] = a.CameWith
			together[a.CameWith] = a.CameWith
		}
	}
	if len(together) == 0 {
		return 0
	}
	meta := gitops.Meta{Principal: comesWithPrincipal, Decision: "the apps were installed together"}
	for _, a := range apps {
		if a.Placed || together[a.Profile] == "" {
			continue
		}
		definition := s.definitionAt(ctx, a)
		if definition == nil {
			continue
		}
		for _, in := range definition.Spec.Integrations {
			if in.Provider == "" || in.Provider == a.Profile || len(in.Capabilities) == 0 || together[in.Provider] != together[a.Profile] {
				continue
			}
			res, err := s.cfg.Repo.PreApproveConsume(ctx, tenant, a.Profile, in.Contract, in.Capabilities, gitops.CameTogetherReason, meta)
			if err != nil {
				log.WarnContext(ctx, "a contract among apps that came together could not be pre-approved",
					"app", a.Profile, "contract", in.Contract, "error", err.Error())
				continue
			}
			if res.Changed {
				approved++
				log.InfoContext(ctx, "pre-approved a contract among apps that came together",
					"app", a.Profile, "contract", in.Contract, "provider", in.Provider, "commit", res.Commit)
			}
		}
	}
	return approved
}

// definitionAt reads the profile of the build a tenant's app is at: the
// build the cluster stored for its pin, or the profile the cluster holds
// where the app has no pin. Nil where neither can be read.
func (s *Server) definitionAt(ctx context.Context, a gitops.App) *gentianov1alpha1.ComponentProfile {
	if a.Digest == "" {
		definition, err := s.cfg.Repo.ProfileDefinition(ctx, a.Profile)
		if err != nil {
			return nil
		}
		return definition
	}
	stored, found, err := s.cfg.Repo.BuildOnCluster(ctx, a.Profile, a.Digest)
	if err != nil || !found {
		return nil
	}
	definition, err := profilebundle.ReadProfile(stored.Body)
	if err != nil {
		return nil
	}
	return definition
}
