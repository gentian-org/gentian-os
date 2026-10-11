/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package customization

import (
	"fmt"
	"sort"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ResolvedAddon is one addon a tenant selected, resolved to the identifier the
// hosting app's own addon system uses.
type ResolvedAddon struct {
	// Profile is the ComponentProfile name the tenant selected (e.g. "odoo-crm-ce").
	Profile string
	// ID is what the app calls it (e.g. the Odoo module "crm"). This is what the
	// composition renders activation for.
	ID string
}

// ResolveAddons maps a tenant's selected addon profile names onto the identifiers
// the hosting app understands, rejecting anything that does not belong.
//
// Resolution is deliberately catalogue-driven: each addon profile declares its own
// spec.customization.addon.{id,of}, so this function never needs to know that Odoo
// calls addons modules or that Nextcloud enables apps. Putting that knowledge in a
// reconciler would violate the platform boundary — gentian-os stays generic and
// app-specific facts live in gentian-apps.
//
// Every problem is reported rather than just the first, so a tenant editing several
// addons at once fixes them in one pass.
func ResolveAddons(
	base *gentianov1alpha1.ComponentProfile,
	selected []string,
	index map[string]*gentianov1alpha1.ComponentProfile,
) ([]ResolvedAddon, []error) {
	if base == nil {
		return nil, []error{fmt.Errorf("base profile is nil")}
	}

	var (
		resolved []ResolvedAddon
		errs     []error
		seenID   = map[string]string{} // addon id -> profile that claimed it
	)
	for _, name := range dedupe(selected) {
		addon, ok := index[name]
		if !ok {
			errs = append(errs, fmt.Errorf("addon %q: no such ComponentProfile", name))
			continue
		}
		// Whether something IS an addon is now the package saying so, rather
		// than an annotation beside a customization block that said it again.
		// One statement, and it is the same one that carries what the app
		// calls this addon and which base it activates into.
		decl := addon.Spec.Package.Addon
		if decl == nil {
			errs = append(errs, fmt.Errorf(
				"addon %q: package.addon is not declared, so this is not an addon — "+
					"only an addon may be selected into an app", name))
			continue
		}

		// An addon activates inside one specific base. Selecting an odoo addon into a
		// nextcloud install would otherwise render an activation the app cannot honour.
		if decl.Of != base.Name {
			errs = append(errs, fmt.Errorf(
				"addon %q activates into %q, not %q", name, decl.Of, base.Name))
			continue
		}
		// Two profiles resolving to the same app-side id would activate the same thing
		// twice — usually a packaging mistake (e.g. a ce and pro profile of one addon
		// selected together), and worth failing on rather than silently deduplicating.
		if prev, dup := seenID[decl.ID]; dup {
			errs = append(errs, fmt.Errorf(
				"addons %q and %q both resolve to id %q", prev, name, decl.ID))
			continue
		}
		seenID[decl.ID] = name

		resolved = append(resolved, ResolvedAddon{Profile: name, ID: decl.ID})
	}

	// Stable order so the rendered XR does not churn on map iteration.
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].Profile < resolved[j].Profile })
	return resolved, errs
}

// AddonIDs returns just the app-side identifiers, in resolution order.
func AddonIDs(resolved []ResolvedAddon) []string {
	ids := make([]string, 0, len(resolved))
	for _, a := range resolved {
		ids = append(ids, a.ID)
	}
	return ids
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
