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
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ValidateRecord applies the rung × scope cost matrix and the per-rung obligations
// to a Customization record. It returns every violation rather than the first, so
// an author fixes the record in one pass.
//
// The check that needs cluster state — does the target profile exist — is
// layered on top by the caller. Whether the app actually runs in the tenant's
// own namespace (docs/app-customization.md §2.4) is not checked anywhere.
func ValidateRecord(record *gentianov1alpha1.Customization) []error {
	if record == nil {
		return nil
	}
	spec := record.Spec
	var errs []error

	if _, known := RungIndex(spec.Rung); !known {
		errs = append(errs, fmt.Errorf("spec.rung %q is not a ladder rung", spec.Rung))
		return errs
	}

	// §3 cost matrix — never fork or patch for a single tenant. A tenant-specific
	// source divergence has no upgrade path and no owner beyond the tenant.
	if spec.Scope == gentianov1alpha1.ScopeTenant && AtOrAbove(spec.Rung, gentianov1alpha1.RungPatch) {
		errs = append(errs, fmt.Errorf(
			"spec.rung %s is not permitted at spec.scope tenant: patching or forking for one tenant "+
				"produces a divergence with no upgrade path (docs/app-customization.md §3)",
			spec.Rung))
	}

	if spec.Scope == gentianov1alpha1.ScopeTenant && len(spec.Tenants) == 0 {
		errs = append(errs, fmt.Errorf("spec.tenants must be non-empty when spec.scope is tenant"))
	}
	if spec.Scope != gentianov1alpha1.ScopeTenant && len(spec.Tenants) > 0 {
		errs = append(errs, fmt.Errorf(
			"spec.tenants must be empty when spec.scope is %s", spec.Scope))
	}

	// P3 upstream-first — mandatory from L4 up, where Gentian starts owning part of
	// the artifact and the delta has to be re-validated at every upstream release.
	if AtOrAbove(spec.Rung, gentianov1alpha1.RungRepackage) {
		if spec.UpstreamFirst == nil || !spec.UpstreamFirst.Attempted {
			errs = append(errs, fmt.Errorf(
				"spec.upstreamFirst.attempted must be true for rung %s "+
					"(docs/app-customization.md P3)", spec.Rung))
		} else if spec.UpstreamFirst.Forwarded == gentianov1alpha1.CustomizationForwarded("no") &&
			strings.TrimSpace(spec.UpstreamFirst.Reason) == "" {
			errs = append(errs, fmt.Errorf(
				"spec.upstreamFirst.reason is required when forwarded is \"no\""))
		}
	}

	// P5 descend over time — a record without exit criteria is a permanent delta
	// that nobody has agreed to make permanent.
	if AtOrAbove(spec.Rung, gentianov1alpha1.RungPatch) && strings.TrimSpace(spec.ExitCriteria) == "" {
		errs = append(errs, fmt.Errorf(
			"spec.exitCriteria is required for rung %s", spec.Rung))
	}

	// Rungs at or above L3 name real artifacts; a record with none is undelivered.
	if AtOrAbove(spec.Rung, gentianov1alpha1.RungExtension) && len(spec.Artifacts) == 0 {
		errs = append(errs, fmt.Errorf(
			"spec.artifacts must name at least one artifact for rung %s", spec.Rung))
	}

	// The justification chain is what stops the ladder being decorative: every
	// cheaper rung must have been considered and rejected in writing.
	errs = append(errs, validateJustification(spec)...)

	return errs
}

// validateJustification requires a written reason for every rung skipped below the
// chosen one.
func validateJustification(spec gentianov1alpha1.CustomizationSpec) []error {
	chosen, ok := RungIndex(spec.Rung)
	if !ok || chosen == 0 {
		return nil
	}
	var errs []error
	for rung, idx := range rungOrder {
		if idx >= chosen {
			continue
		}
		// L2 is always available, so skipping it always needs a reason; the others
		// only need one if the app actually supports them, which the caller checks.
		if reason, present := spec.RungJustification[string(rung)]; !present || strings.TrimSpace(reason) == "" {
			errs = append(errs, fmt.Errorf(
				"spec.rungJustification[%q] is required: state why %s cannot express this change", rung, rung))
		}
	}
	return errs
}

// ValidateAgainstSurface checks a record against the target app's declared
// customization surface. Separated from ValidateRecord because it needs the
// ComponentProfile, which is not always available to the caller.
func ValidateAgainstSurface(
	record *gentianov1alpha1.Customization,
	surface *gentianov1alpha1.CustomizationSurface,
) []error {
	if record == nil {
		return nil
	}
	var errs []error
	rung := record.Spec.Rung

	if !SupportsRung(surface, rung) {
		errs = append(errs, fmt.Errorf(
			"target app does not support rung %s (spec.customization.supportedRungs)", rung))
	}

	// Flag a record sitting above a rung the app actually supports — the signal
	// that a customization could be descended.
	if lower, found := lowerSupportedRung(surface, rung); found {
		errs = append(errs, fmt.Errorf(
			"target app supports rung %s, which is cheaper than %s: justify or descend", lower, rung))
	}
	return errs
}

// lowerSupportedRung returns the cheapest supported rung strictly below rung.
func lowerSupportedRung(
	surface *gentianov1alpha1.CustomizationSurface,
	rung gentianov1alpha1.CustomizationRung,
) (gentianov1alpha1.CustomizationRung, bool) {
	chosen, ok := RungIndex(rung)
	if !ok {
		return "", false
	}
	best := gentianov1alpha1.CustomizationRung("")
	bestIdx := -1
	for candidate, idx := range rungOrder {
		if idx >= chosen || !SupportsRung(surface, candidate) {
			continue
		}
		if bestIdx == -1 || idx < bestIdx {
			best, bestIdx = candidate, idx
		}
	}
	return best, bestIdx != -1
}
