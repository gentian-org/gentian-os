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

// Package customization implements the Gentian customization ladder: rung
// ordering, per-app surface resolution, and the policy rules that decide whether
// a given (rung, scope) pair is permitted.
//
// The package is deliberately app-neutral. It knows about rungs and scopes, never
// about individual catalogue entries — per-app behaviour comes from the
// AppProfile's declared customization surface. See docs/app-customization.md.
package customization

import (
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// DefaultDropInMaxBytes caps tenant-supplied drop-in content when the AppProfile
// does not set an explicit limit. ConfigMaps are limited to ~1MiB in total, and
// drop-ins are meant for fragments, not payloads.
const DefaultDropInMaxBytes = 262144

// rungOrder maps each rung to its position on the ladder. Lower is cheaper: the
// ordering is by how much of the app's own delivery artifact Gentian ends up
// owning, which is what determines upgrade cost.
var rungOrder = map[gentianov1alpha1.CustomizationRung]int{
	gentianov1alpha1.RungConfigure: 0,
	gentianov1alpha1.RungDropIn:    1,
	gentianov1alpha1.RungCompanion: 2,
	gentianov1alpha1.RungExtension: 3,
	gentianov1alpha1.RungRepackage: 4,
	gentianov1alpha1.RungPatch:     5,
	gentianov1alpha1.RungFork:      6,
}

// RungIndex returns the ladder position of a rung and whether it is a known rung.
func RungIndex(rung gentianov1alpha1.CustomizationRung) (int, bool) {
	idx, ok := rungOrder[rung]
	return idx, ok
}

// AtOrAbove reports whether rung sits at or above ref on the ladder. Used for the
// obligation gates: "rung >= L4 requires an upstream-first record", and so on.
func AtOrAbove(rung, ref gentianov1alpha1.CustomizationRung) bool {
	r, okR := rungOrder[rung]
	f, okF := rungOrder[ref]
	if !okR || !okF {
		return false
	}
	return r >= f
}

// DefaultSupportedRungs is what consumers must assume for an app with no declared
// customization surface: it can be configured or repackaged, and nothing may be
// inferred beyond that. L2 is always available and is never listed (see
// SupportsRung).
func DefaultSupportedRungs() []gentianov1alpha1.CustomizationRung {
	return []gentianov1alpha1.CustomizationRung{
		gentianov1alpha1.RungConfigure,
		gentianov1alpha1.RungRepackage,
	}
}

// SupportsRung reports whether an app supports being customized at the given rung.
//
// L2 (companion) is always true: a companion is a separate deployable built
// against whatever the target publishes, so its availability is a property of the
// customization rather than of the target. That is why L2 never appears in
// spec.customization.supportedRungs.
func SupportsRung(surface *gentianov1alpha1.CustomizationSurface, rung gentianov1alpha1.CustomizationRung) bool {
	if rung == gentianov1alpha1.RungCompanion {
		return true
	}
	supported := DefaultSupportedRungs()
	if surface != nil && len(surface.SupportedRungs) > 0 {
		supported = surface.SupportedRungs
	}
	for _, candidate := range supported {
		if candidate == rung {
			return true
		}
	}
	return false
}

// LowestSupportedRung returns the cheapest rung at or above min that the app
// supports. It encodes principle P1 ("lowest viable rung") for callers that have
// already determined which rungs could express the change.
func LowestSupportedRung(
	surface *gentianov1alpha1.CustomizationSurface,
	candidates []gentianov1alpha1.CustomizationRung,
) (gentianov1alpha1.CustomizationRung, bool) {
	best := gentianov1alpha1.CustomizationRung("")
	bestIdx := -1
	for _, candidate := range candidates {
		idx, ok := rungOrder[candidate]
		if !ok || !SupportsRung(surface, candidate) {
			continue
		}
		if bestIdx == -1 || idx < bestIdx {
			best, bestIdx = candidate, idx
		}
	}
	return best, bestIdx != -1
}

// FindDropIn returns the declared drop-in with the given name, or nil.
func FindDropIn(
	surface *gentianov1alpha1.CustomizationSurface,
	name string,
) *gentianov1alpha1.CustomizationDropIn {
	if surface == nil {
		return nil
	}
	for i := range surface.DropIns {
		if surface.DropIns[i].Name == name {
			return &surface.DropIns[i]
		}
	}
	return nil
}

// GradeForScore bands a rubric score into a readiness grade (§4.1). Assignment is
// manual for v0.4 — this exists so CI can check that a declared grade and its
// recorded score agree, not to compute the grade unattended.
func GradeForScore(score int32) gentianov1alpha1.CustomizationGrade {
	switch {
	case score >= 7:
		return gentianov1alpha1.GradeA
	case score >= 5:
		return gentianov1alpha1.GradeB
	case score >= 3:
		return gentianov1alpha1.GradeC
	default:
		return gentianov1alpha1.GradeD
	}
}
