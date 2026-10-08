/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
	"github.com/gentian-org/gentian-os/internal/tenancy"
)

// What the catalogue left behind of one app, as a tenant that has the app
// sees it.
//
// The list (residue.go) is the cluster's: a profile is on a cluster once,
// under one name, for every tenant that installed it, and so is what its
// bundle brought. A tenant's administrator is shown the part of it that is
// about an app the tenant has -- what a newer build of that app, or of an
// add-on switched on inside it, dropped -- and nothing else of it: not what
// no bundle owns, not the profiles nobody uses, and nothing about a profile
// the tenant does not have, which would be a way to read the cluster's
// catalogue.
//
// Removing is the cluster administrator's, with one exception. On a cluster
// whose tenancy mode is single there is one user tenant, the pieces are
// shared with no other, and its administrator may remove them. It is the same
// removal -- the list worked out again, one object, deleted as it was checked
// -- with more refused: only a leftover of an app the tenant has.

// Who may remove what the read lists.
const (
	RemovableByTenant   = "tenant"
	RemovableByPlatform = "platform"
)

// ErrNotInstalled is a question about an app the tenant does not have, or
// about a tenant that is not there. One answer for both.
var ErrNotInstalled = errors.New("not installed in this tenant")

// ErrSharedResidue is a removal asked for a tenant on a cluster where the
// pieces are every tenant's. Nothing is deleted. What the person is told is
// tenancy.SharedPieces.
var ErrSharedResidue = errors.New("the pieces are shared by every tenant that uses the app")

// AppResidue is the answer of the read for one app of one tenant.
type AppResidue struct {
	Tenant  string `json:"tenant"`
	Profile string `json:"profile"`
	// Profiles are the profiles the list is about: the one asked for and,
	// for an app, the add-ons switched on inside it.
	Profiles []string `json:"profiles"`
	// Residue holds the leftovers that name one of them, of the classes
	// dropped and orphaned, as the cluster's list has them.
	Residue []ResidueItem `json:"residue"`
	// RemovableBy is who may remove an item of it: "tenant" where this
	// tenant is the cluster's only user tenant, "platform" everywhere else.
	RemovableBy string `json:"removableBy"`
	// Incomplete says what could not be established about these profiles,
	// and so what the list may be missing.
	Incomplete []string `json:"incomplete"`
	OIDCRule   string   `json:"oidcRule"`
}

// ResidueScope narrows a removal to what a tenant's administrator may
// remove: a leftover of one profile the tenant has installed.
type ResidueScope struct {
	Tenant  string
	Profile string
}

// holds reports whether an item of the list is in the scope: what a newer
// build of the profile dropped, or what names it when it is gone. An object
// no bundle owns, and a profile nobody uses, never are.
func (s *ResidueScope) holds(item ResidueItem) bool {
	return tenantsClass(item.Class) && item.Profile != "" && item.Profile == s.Profile
}

func tenantsClass(class string) bool {
	return class == ResidueDropped || class == ResidueOrphaned
}

// removableBy says who may remove an app's leftovers in a tenant.
func (s *Service) removableBy(tenant string) string {
	if tenancy.SoleUserTenant(s.opts.TenancyMode, tenant) {
		return RemovableByTenant
	}
	return RemovableByPlatform
}

// installedAs answers the profiles a question about one profile of a tenant
// is about: the profile itself and, when it is an app, the add-ons switched
// on inside it. Empty when the tenant has it neither as an app nor as an
// add-on that is switched on.
//
// Read from the Tenant as the cluster holds it, which is what the profile's
// pieces are rendered for. A pin on an add-on that is not switched on
// installs nothing, and does not count.
func installedAs(tenant *gentianov1alpha1.Tenant, profile string) []string {
	if profile == "" {
		return nil
	}
	var asAddon bool
	for _, app := range tenant.Spec.Apps {
		name := app.Profile
		if name == "" && app.ProfileRef != nil {
			name = app.ProfileRef.Name
		}
		if name == profile {
			out := []string{profile}
			for _, addon := range app.Addons {
				if addon != "" && !slices.Contains(out, addon) {
					out = append(out, addon)
				}
			}
			return out
		}
		asAddon = asAddon || slices.Contains(app.Addons, profile)
	}
	if asAddon {
		return []string{profile}
	}
	return nil
}

// tenantHas reads the tenant from the API server and answers installedAs,
// or ErrNotInstalled.
func (s *Service) tenantHas(ctx context.Context, tenantName, profile string) ([]string, error) {
	var tenant gentianov1alpha1.Tenant
	if err := s.live().Get(ctx, types.NamespacedName{Name: tenantName}, &tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s in %s", ErrNotInstalled, profile, tenantName)
		}
		return nil, fmt.Errorf("get tenant %s: %w", tenantName, err)
	}
	profiles := installedAs(&tenant, profile)
	if len(profiles) == 0 {
		return nil, fmt.Errorf("%w: %s in %s", ErrNotInstalled, profile, tenantName)
	}
	return profiles, nil
}

// AppResidue lists what a newer build left behind of one app a tenant has.
func (s *Service) AppResidue(ctx context.Context, tenantName, profile string) (*AppResidue, error) {
	profiles, err := s.tenantHas(ctx, tenantName, profile)
	if err != nil {
		return nil, err
	}
	// Without the profiles nobody uses: none of them is this tenant's, and
	// listing them reads what every tenant retains.
	view, err := s.residue(ctx, false)
	if err != nil {
		return nil, err
	}
	out := &AppResidue{
		Tenant: tenantName, Profile: profile, Profiles: profiles,
		Residue: []ResidueItem{}, Incomplete: []string{},
		RemovableBy: s.removableBy(tenantName), OIDCRule: oidcRule,
	}
	for _, f := range view.items {
		if tenantsClass(f.item.Class) && slices.Contains(profiles, f.item.Profile) {
			out.Residue = append(out.Residue, f.item)
		}
	}
	// What the cluster's list could not establish names other profiles and
	// other tenants. Only what is about these profiles is said here.
	for _, name := range profiles {
		if state := view.profiles[name]; state != nil && state.unreadable != nil {
			out.Incomplete = append(out.Incomplete, unreadableBundle(name, state.unreadable))
		}
	}
	return out, nil
}

// RemoveAppResidue deletes one leftover of an app for a tenant's
// administrator. It is RemoveResidue with a scope, and deletes nothing that
// would not: see admitsScope and ResidueScope.holds for what the scope adds.
func (s *Service) RemoveAppResidue(ctx context.Context, scope ResidueScope, kind, name, namespace, actor string) (*RemoveResidueResult, error) {
	return s.removeResidue(ctx, kind, name, namespace, actor, &scope)
}

// admitsScope is what is asked of a tenant's removal before the list is
// looked at: that the pieces are this tenant's alone, that the tenant has the
// profile, and that the object is not a profile.
func (s *Service) admitsScope(ctx context.Context, scope *ResidueScope, kind, name string) error {
	if s.removableBy(scope.Tenant) != RemovableByTenant {
		return ErrSharedResidue
	}
	if _, err := s.tenantHas(ctx, scope.Tenant, scope.Profile); err != nil {
		return err
	}
	if kind == profilebundle.KindProfile {
		return fmt.Errorf("%w: the ComponentProfile %s is the cluster's, and is not removed for a tenant. Nothing was deleted",
			ErrNotResidue, name)
	}
	return nil
}
