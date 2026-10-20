/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package platformversion is the one place the platform's own version line
// is stated, and the floor a profile build declares against it.
//
// A version line is the major and minor of a release: 0.5 for 0.5.0,
// 0.5.3 and 0.5.0-dev alike. It is what a build of an app is compatible
// with, because the contract between a profile and the platform -- the
// fields the operator reads, the objects a bundle may bring, what the
// platform injects -- moves with the line and not with a patch. A profile
// declares the oldest line it runs on, spec.requires.platform, and a
// cluster on an older line refuses it. There is no upper bound: what a
// later line stops reading, the platform says itself, one line ahead.
package platformversion

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a version line, major and minor.
type Version struct {
	Major, Minor int
}

// This is Line, parsed.
func This() Version {
	v, _ := Parse(Line)
	return v
}

// Parse reads the line of a version: "0.5", "v0.5", "0.5.2" and "0.5.0-dev"
// are all line 0.5. ok is false for anything else, and for "".
func Parse(s string) (Version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "+-"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return Version{}, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return Version{}, false
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1]}, true
}

// Compare is -1, 0 or 1 as v is older than, the same as or newer than o.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		if v.Major < o.Major {
			return -1
		}
		return 1
	case v.Minor < o.Minor:
		return -1
	case v.Minor > o.Minor:
		return 1
	}
	return 0
}

func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor)
}

// Floor is what a profile build requires of the platform: a version line it
// and every later one satisfy.
type Floor struct {
	// Min is the oldest line admitted.
	Min Version
	// Stated is false for a profile that declares nothing, which is a floor
	// of 0.0: every platform admits it.
	Stated bool
}

// ParseFloor reads spec.requires.platform: ">=0.5", or "0.5", which means
// the same. A patch component is not a floor's to state -- the contract
// moves with the line -- and is refused, as the CRD's pattern refuses it,
// so that a profile read from somewhere the schema did not run is held to
// the same rule. "" is the floor that admits everything.
func ParseFloor(s string) (Floor, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Floor{}, nil
	}
	rest := strings.TrimPrefix(s, ">=")
	rest = strings.TrimPrefix(rest, "v")
	parts := strings.Split(rest, ".")
	if len(parts) != 2 {
		return Floor{}, fmt.Errorf("platform floor %q is not a version line: write \">=<major>.<minor>\", for example \">=%s\"", s, Line)
	}
	var nums [2]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return Floor{}, fmt.Errorf("platform floor %q is not a version line: write \">=<major>.<minor>\", for example \">=%s\"", s, Line)
		}
		nums[i] = n
	}
	return Floor{Min: Version{Major: nums[0], Minor: nums[1]}, Stated: true}, nil
}

// Admits answers whether a platform on line v may install a build with this
// floor.
func (f Floor) Admits(v Version) bool {
	return !f.Stated || f.Min.Compare(v) <= 0
}

// String is the floor as a profile writes it; "" for one that states nothing.
func (f Floor) String() string {
	if !f.Stated {
		return ""
	}
	return ">=" + f.Min.String()
}

// Unmet is the sentence a refusal or a listing carries when a build's floor
// is above this platform's line, and "" when it is admitted. floor is
// spec.requires.platform as written; one that cannot be read is reported as
// unmet, with why, because a build that states something unreadable is not
// one this platform knows it can run.
func Unmet(floor string) string {
	f, err := ParseFloor(floor)
	if err != nil {
		return err.Error()
	}
	if f.Admits(This()) {
		return ""
	}
	return fmt.Sprintf("needs platform %s; this cluster is %s", f.Min, This())
}
