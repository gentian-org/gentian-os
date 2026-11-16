/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The platform release a cluster runs is declared on the Cluster claim
// (spec.release, docs/concepts/versioning.md §2): a release tag the cluster
// runs exactly, or a branch it follows build by build, with how far the
// director may move a version on its own. The director is the one writer
// of that block, as of everything in the deployments repository: a person's
// choice and the auto-upgrade mover both come through SetRelease, and the
// catalog the cluster reads moves with the release in the same commit.

// ReleaseSpec is spec.release as declared.
type ReleaseSpec struct {
	// Track is a branch the cluster follows; "" when it runs a version.
	Track string `json:"track,omitempty"`
	// Version is the release tag the cluster runs; "" when it follows a branch.
	Version string `json:"version,omitempty"`
	// AutoUpgrade is none, patch or minor; "" reads as none. Only with Version.
	AutoUpgrade string `json:"autoUpgrade,omitempty"`
}

// Declared says whether the claim names a release at all.
func (r ReleaseSpec) Declared() bool { return r.Track != "" || r.Version != "" }

const (
	// GentianCatalogueSource is the name of the catalog source the installer
	// writes for the gentian-apps catalog; it follows the release.
	GentianCatalogueSource = "gentian"
	// ReleasedCatalogueURL is the catalog a cluster on a release reads.
	ReleasedCatalogueURL = "https://gentian-org.github.io/gentian-apps"
	// DevelopmentCatalogueURL is the catalog a cluster on a branch reads.
	DevelopmentCatalogueURL = "https://gentian-org.github.io/gentian-apps/develop"
)

var (
	// ErrInvalidRelease is a spec that names both or neither, a version
	// that is not a tag, a branch that is not a ref, or a mode with a track.
	ErrInvalidRelease = errors.New("invalid release")

	releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	releaseTrack   = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	releaseModes   = map[string]bool{"": true, "none": true, "patch": true, "minor": true}
)

// CheckRelease holds a spec to what the claim's schema admits, before
// anything is committed: the same rules as the XRD's, so a refusal comes
// from here with a reason and not from Argo CD an hour later.
func CheckRelease(r ReleaseSpec) error {
	switch {
	case r.Track == "" && r.Version == "":
		return fmt.Errorf("%w: name a version (v0.5.1) or a track (a branch)", ErrInvalidRelease)
	case r.Track != "" && r.Version != "":
		return fmt.Errorf("%w: a cluster runs a version or follows a track, not both", ErrInvalidRelease)
	case r.Version != "" && !releaseVersion.MatchString(r.Version):
		return fmt.Errorf("%w: %q is not a release tag of the form vMAJOR.MINOR.PATCH", ErrInvalidRelease, r.Version)
	case r.Track != "" && !releaseTrack.MatchString(r.Track):
		return fmt.Errorf("%w: %q is not a branch name", ErrInvalidRelease, r.Track)
	case r.Track != "" && r.AutoUpgrade != "":
		return fmt.Errorf("%w: autoUpgrade has no meaning with a track; a cluster on a branch follows every build of it", ErrInvalidRelease)
	case !releaseModes[r.AutoUpgrade]:
		return fmt.Errorf("%w: autoUpgrade is none, patch or minor, not %q", ErrInvalidRelease, r.AutoUpgrade)
	}
	return nil
}

// CatalogueFor is the gentian-apps catalog a cluster on this release reads:
// the released one for a version, the development one for a branch.
func CatalogueFor(r ReleaseSpec) string {
	if r.Version != "" {
		return ReleasedCatalogueURL
	}
	return DevelopmentCatalogueURL
}

// Release reads spec.release from the claim in git.
func (g *GitOps) Release(ctx context.Context) (ReleaseSpec, error) {
	var claim struct {
		Spec struct {
			Release ReleaseSpec `json:"release"`
		} `json:"spec"`
	}
	if err := g.readClusterClaim(ctx, &claim); err != nil {
		return ReleaseSpec{}, err
	}
	return claim.Spec.Release, nil
}

// SetRelease writes spec.release and points the gentian catalog source at
// the release's catalog, in one signed commit. A claim that says so already
// is left alone.
func (g *GitOps) SetRelease(ctx context.Context, want ReleaseSpec, meta Meta) (Result, error) {
	if err := CheckRelease(want); err != nil {
		return Result{}, err
	}
	what := want.Version
	if what == "" {
		what = "track " + want.Track
	}
	message := fmt.Sprintf("feat(cluster): release %s (via %s)", what, meta.actor())
	return g.applyClaim(ctx, message, meta, func(text string) (string, string, bool, error) {
		out, changed, err := releaseEdit(text, want)
		if err != nil {
			return text, "", false, err
		}
		if !changed {
			return text, "unchanged", false, nil
		}
		return out, "updated", true, nil
	})
}

// releaseEdit is the edit SetRelease commits: the release block, and the
// gentian catalog source's address where the claim has that source.
func releaseEdit(text string, want ReleaseSpec) (string, bool, error) {
	out, changed, err := setKeyPath(text, "spec", []string{"release"}, releaseLeaf(want))
	if errors.Is(err, errNoRoot) {
		return text, false, fmt.Errorf("the claim has no spec block")
	}
	if err != nil {
		return text, false, err
	}
	sources, err := claimCatalogueSources(out)
	if err != nil {
		return text, false, err
	}
	catalogue := CatalogueFor(want)
	for i, src := range sources {
		if src.Name != GentianCatalogueSource || src.URL == catalogue || src.URL == "" {
			continue
		}
		sources[i].URL = catalogue
		out, err = setClaimSources(out, sources)
		if err != nil {
			return text, false, err
		}
		changed = true
	}
	// The sources writer ends the file where the list ends; the file keeps
	// ending in a newline, as every claim the installer wrote does.
	if changed && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out, changed, nil
}

// releaseLeaf writes spec.release as a block: the key and, under it, track
// or version and the mode where one is set. It replaces the block whole, so
// a track that becomes a version leaves no track line behind.
type releaseLeaf ReleaseSpec

func (r releaseLeaf) children() []string {
	var out []string
	if r.Track != "" {
		out = append(out, "track: "+quoteScalar(r.Track))
	}
	if r.Version != "" {
		out = append(out, "version: "+quoteScalar(r.Version))
	}
	if r.AutoUpgrade != "" && r.AutoUpgrade != "none" {
		out = append(out, "autoUpgrade: "+r.AutoUpgrade)
	}
	return out
}

func (r releaseLeaf) render(indent int, key string) []string {
	pad := strings.Repeat(" ", indent)
	out := []string{pad + key + ":"}
	for _, c := range r.children() {
		out = append(out, pad+"  "+c)
	}
	return out
}

func (releaseLeaf) extent(lines []string, at, indent int) int {
	return listLeaf(nil).extent(lines, at, indent)
}

func (r releaseLeaf) same(lines []string, at, indent int) bool {
	var cur []string
	for i := at + 1; i < at+r.extent(lines, at, indent) && i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k := strings.Index(t, " #"); k >= 0 {
			t = strings.TrimSpace(t[:k])
		}
		cur = append(cur, t)
	}
	want := r.children()
	if len(cur) != len(want) {
		return false
	}
	for i := range cur {
		if normalizeScalarLine(cur[i]) != normalizeScalarLine(want[i]) {
			return false
		}
	}
	return true
}

// normalizeScalarLine compares "key: value" lines regardless of quoting.
func normalizeScalarLine(s string) string {
	k, v, ok := strings.Cut(s, ":")
	if !ok {
		return s
	}
	return strings.TrimSpace(k) + ": " + strings.Trim(strings.TrimSpace(v), `"'`)
}
