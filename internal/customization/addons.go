/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
	// Profile is the AppProfile name the tenant selected (e.g. "odoo-crm-ce").
	Profile string
	// ID is what the app calls it (e.g. the Odoo module "crm"). This is what the
	// composition renders activation for.
	ID string
	// RequiresEntitlement reports whether the addon is commercially licensed, so
	// the caller can gate activation on an install grant.
	RequiresEntitlement bool
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
	base *gentianov1alpha1.AppProfile,
	selected []string,
	index map[string]*gentianov1alpha1.AppProfile,
) ([]ResolvedAddon, []error) {
	if base == nil {
		return nil, []error{fmt.Errorf("base profile is nil")}
	}

	var (
		resolved []ResolvedAddon
		errs     []error
		seenID   = map[string]string{} // addon id -> profile that claimed it
	)
	baseFamily := gentianov1alpha1.ProfileFamily(base)

	for _, name := range dedupe(selected) {
		addon, ok := index[name]
		if !ok {
			errs = append(errs, fmt.Errorf("addon %q: no such AppProfile", name))
			continue
		}
		if gentianov1alpha1.EffectiveDeploymentRole(addon) != gentianov1alpha1.ProfileDeploymentRoleAddon {
			errs = append(errs, fmt.Errorf(
				"addon %q: deployment-role is %q, not addon — only addons may be selected into an app",
				name, gentianov1alpha1.EffectiveDeploymentRole(addon)))
			continue
		}

		decl := addonDecl(addon)
		if decl == nil {
			errs = append(errs, fmt.Errorf(
				"addon %q: spec.customization.addon is not declared, so the operator "+
					"cannot know what the app calls it", name))
			continue
		}

		// An addon activates inside one specific base. Selecting an odoo addon into a
		// nextcloud install would otherwise render an activation the app cannot honour.
		if decl.Of != base.Name {
			errs = append(errs, fmt.Errorf(
				"addon %q activates into %q, not %q", name, decl.Of, base.Name))
			continue
		}
		if family := gentianov1alpha1.ProfileFamily(addon); family != baseFamily {
			errs = append(errs, fmt.Errorf(
				"addon %q is family %q but %q is family %q", name, family, base.Name, baseFamily))
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

		resolved = append(resolved, ResolvedAddon{
			Profile:             name,
			ID:                  decl.ID,
			RequiresEntitlement: gentianov1alpha1.ProfileRequiresEntitlement(addon),
		})
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

// EntitledAddons splits resolved addons into those that may be activated now and
// those blocked pending an install grant. Entitlement — not technical
// compatibility — is what gates a commercial addon; see the editions model in
// gentian-apps/docs/L3-cleanup.md §2.1.
func EntitledAddons(resolved []ResolvedAddon, granted map[string]bool) (allowed, blocked []ResolvedAddon) {
	for _, a := range resolved {
		if a.RequiresEntitlement && !granted[a.Profile] {
			blocked = append(blocked, a)
			continue
		}
		allowed = append(allowed, a)
	}
	return allowed, blocked
}

func addonDecl(p *gentianov1alpha1.AppProfile) *gentianov1alpha1.CustomizationAddon {
	if p == nil || p.Spec.Customization == nil {
		return nil
	}
	return p.Spec.Customization.Addon
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
