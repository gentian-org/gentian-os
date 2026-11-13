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

// companion is one app to install with another: where its build is, or no
// catalog where the cluster already holds its profile for this tenant.
type companion struct {
	name, catalogue, digest string
}

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
	if definition == nil || len(definition.Spec.ComesWith) == 0 {
		return nil, true
	}
	ctx := r.Context()
	apps, err := s.cfg.Repo.Apps(ctx, tenant)
	if err != nil && !errors.Is(err, gitops.ErrTenantNotFound) {
		s.repoError(w, r, err)
		return nil, false
	}
	var plan []companion
	for _, name := range definition.Spec.ComesWith {
		if name == profile || slices.ContainsFunc(apps, func(a gitops.App) bool { return a.Profile == name }) {
			continue
		}
		found, why, err := s.findCompanion(ctx, tenant, name, preferred)
		if err != nil {
			s.repoError(w, r, err)
			return nil, false
		}
		if why != "" {
			s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
				"%s comes with %s, and %s. Nothing was installed.", profile, name, why))
			return nil, false
		}
		plan = append(plan, found)
	}
	var came []string
	for _, p := range plan {
		if p.catalogue != "" {
			if _, detail, err := s.materialise(ctx, c, "app", tenant, p.name, p.catalogue, p.digest); err != nil {
				s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf(
					"%s comes with %s, which cannot be installed here: %s", profile, p.name, detail))
				return nil, false
			}
		}
		res, err := s.cfg.Repo.InstallWith(ctx, tenant, p.name, p.digest, p.catalogue, profile, defaultGrant, c.meta)
		if err != nil {
			s.repoError(w, r, err)
			return nil, false
		}
		if res.Changed {
			came = append(came, p.name)
		}
	}
	return came, true
}

// findCompanion answers where the build of an app that comes with another
// is: the catalog the other app is from first, then the others the tenant
// sees, then the profile the cluster already holds for this tenant. why says
// in words that it was found nowhere.
func (s *Server) findCompanion(ctx context.Context, tenant, name, preferred string) (companion, string, error) {
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
