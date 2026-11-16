/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package releaseindex reads the platform's release index,
// docs/releases/index.yaml as published on main: one entry per release, and
// what a cluster needs to decide whether its mode may take it. The operator
// reads it to say which release is newest and to resolve a release to the
// UI charts and the catalog that belong to it; the director reads it to move
// a cluster by its autoUpgrade mode.
package releaseindex

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/envvar"
)

// Release is one entry of the index.
type Release struct {
	// Version is the tag, "v0.5.1".
	Version string `json:"version"`
	// Line is the version line, "0.5".
	Line string `json:"line"`
	// Date is the day the release was cut, "2026-11-13".
	Date string `json:"date,omitempty"`
	// Notes is where the release notes are.
	Notes string `json:"notes,omitempty"`
	// UpgradesFrom lists the version lines a cluster moves from in place.
	// Empty: the release is installed fresh and never taken automatically.
	UpgradesFrom []string `json:"upgradesFrom"`
	// UI names the gentian-ui chart versions of this release by component:
	// desktop, adminConsole, concierge. Absent before gentian-ui published
	// charts under its tags.
	UI map[string]string `json:"ui,omitempty"`
	// Catalogue is the catalog a cluster on this release reads.
	Catalogue string `json:"catalogue,omitempty"`
}

// Index is the release index.
type Index struct {
	Releases []Release `json:"releases"`
}

// Mode is a cluster's autoUpgrade mode.
type Mode string

const (
	// ModeNone takes nothing by itself.
	ModeNone Mode = "none"
	// ModePatch takes the next patch of the cluster's line.
	ModePatch Mode = "patch"
	// ModeMinor also takes the next minor, where the release is applied in place.
	ModeMinor Mode = "minor"
)

// Version is a parsed release number.
type Version struct {
	Major, Minor, Patch int
	// Prerelease is what follows the first "-", "" for a release.
	Prerelease string
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

// ParseVersion reads "v0.5.1", "0.5.1" or "0.6.0-dev".
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("%q is not a version of the form vMAJOR.MINOR.PATCH", s)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return Version{Major: major, Minor: minor, Patch: patch, Prerelease: m[4]}, nil
}

// Line is the version line, "0.5".
func (v Version) Line() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Compare orders two versions; a prerelease sorts before the release of the
// same number.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return sign(v.Major - o.Major)
	case v.Minor != o.Minor:
		return sign(v.Minor - o.Minor)
	case v.Patch != o.Patch:
		return sign(v.Patch - o.Patch)
	case v.Prerelease == o.Prerelease:
		return 0
	case v.Prerelease == "":
		return 1
	case o.Prerelease == "":
		return -1
	default:
		return strings.Compare(v.Prerelease, o.Prerelease)
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// Parse reads the index and checks every entry: a version of the right
// form, a line that is the version's, no duplicate. The order in the file
// does not matter; Releases comes back sorted, newest last.
func Parse(text []byte) (Index, error) {
	var idx Index
	if err := yaml.Unmarshal(text, &idx); err != nil {
		return Index{}, fmt.Errorf("release index: %w", err)
	}
	seen := map[string]bool{}
	for i := range idx.Releases {
		r := &idx.Releases[i]
		v, err := ParseVersion(r.Version)
		if err != nil {
			return Index{}, fmt.Errorf("release index: %w", err)
		}
		if r.Line == "" {
			r.Line = v.Line()
		} else if r.Line != v.Line() {
			return Index{}, fmt.Errorf("release index: %s states line %q, its version's is %q", r.Version, r.Line, v.Line())
		}
		if !strings.HasPrefix(r.Version, "v") {
			r.Version = "v" + r.Version
		}
		if seen[r.Version] {
			return Index{}, fmt.Errorf("release index: %s is listed twice", r.Version)
		}
		seen[r.Version] = true
		if r.UpgradesFrom == nil {
			r.UpgradesFrom = []string{}
		}
	}
	sort.SliceStable(idx.Releases, func(i, j int) bool {
		a, _ := ParseVersion(idx.Releases[i].Version)
		b, _ := ParseVersion(idx.Releases[j].Version)
		return a.Compare(b) < 0
	})
	return idx, nil
}

// DefaultFeed is the index as published on gentian-os's main.
const DefaultFeed = "https://raw.githubusercontent.com/gentian-org/gentian-os/main/docs/releases/index.yaml"

// FeedFromEnv is where the operator reads the release index from:
// GENTIAN_RELEASE_FEED_URL, DefaultFeed when unset, and nothing ("") for
// "none", an air-gapped cluster.
func FeedFromEnv() string {
	feed := envvar.Or("GENTIAN_RELEASE_FEED_URL", DefaultFeed)
	if feed == "none" {
		return ""
	}
	return feed
}

// Fetch reads the index from its address. The body is capped at one MiB.
func Fetch(ctx context.Context, client *http.Client, url string) (Index, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Index{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return Index{}, fmt.Errorf("read the release index at %s: %w", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return Index{}, fmt.Errorf("read the release index at %s: HTTP %d", url, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Index{}, fmt.Errorf("read the release index at %s: %w", url, err)
	}
	return Parse(body)
}

// Find returns the entry of a version, with or without its "v".
func (idx Index) Find(version string) (Release, bool) {
	want, err := ParseVersion(version)
	if err != nil {
		return Release{}, false
	}
	for _, r := range idx.Releases {
		if v, _ := ParseVersion(r.Version); v.Compare(want) == 0 {
			return r, true
		}
	}
	return Release{}, false
}

// Newest is the newest release of the index; prereleases do not count.
func (idx Index) Newest() (Release, bool) {
	for i := len(idx.Releases) - 1; i >= 0; i-- {
		if v, _ := ParseVersion(idx.Releases[i].Version); v.Prerelease == "" {
			return idx.Releases[i], true
		}
	}
	return Release{}, false
}

// InPlaceFrom says whether a cluster on the given line may move to this
// release in place.
func (r Release) InPlaceFrom(line string) bool {
	for _, l := range r.UpgradesFrom {
		if l == line {
			return true
		}
	}
	return false
}

// Next decides what a cluster running `current` does under a mode: `take` is
// the release the mode moves to by itself, if any, and `pending` the newest
// release the mode does not take and a person has to decide about. A release
// that is not applied in place from the cluster's line is never taken;
// a prerelease never counts; a release older than or equal to current is
// nothing.
//
// Under ModeMinor a cluster two lines behind moves one line at a time: to
// the newest release that is in place from its line, then on from there.
func (idx Index) Next(current string, mode Mode) (take, pending *Release) {
	cur, err := ParseVersion(current)
	if err != nil {
		return nil, nil
	}
	line := cur.Line()
	var newer []Release
	for _, r := range idx.Releases {
		v, _ := ParseVersion(r.Version)
		if v.Prerelease != "" || v.Compare(cur) <= 0 {
			continue
		}
		newer = append(newer, r)
	}
	if len(newer) == 0 {
		return nil, nil
	}
	for i := len(newer) - 1; i >= 0; i-- {
		r := newer[i]
		v, _ := ParseVersion(r.Version)
		allowed := false
		switch mode {
		case ModePatch:
			allowed = v.Line() == line && r.InPlaceFrom(line)
		case ModeMinor:
			allowed = v.Major == cur.Major && r.InPlaceFrom(line)
		}
		if allowed {
			take = &newer[i]
			break
		}
	}
	newest := newer[len(newer)-1]
	if take == nil || take.Version != newest.Version {
		pending = &newest
	}
	return take, pending
}
