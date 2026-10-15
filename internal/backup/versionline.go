/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"strconv"
	"strings"
)

// PlatformVersionLine is the version line of this platform: the major and
// minor version of the release it is built towards. An export writes it
// into the manifest (operatorVersion); a restore compares a bundle's with it.
// It moves when a release starts a new line, with the chart's version.
const PlatformVersionLine = "0.5"

// VersionLine is a platform version line, major and minor.
type VersionLine struct {
	Major, Minor int
}

// ParseVersionLine reads the line of a version: "0.5", "v0.5", "0.5.2" and
// "0.5.2+abc" are all line 0.5. ok is false for anything else, and for "".
func ParseVersionLine(s string) (VersionLine, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "+-"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return VersionLine{}, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return VersionLine{}, false
		}
		nums[i] = n
	}
	return VersionLine{Major: nums[0], Minor: nums[1]}, true
}

// Compare is -1, 0 or 1 as l is older than, the same as or newer than o.
func (l VersionLine) Compare(o VersionLine) int {
	switch {
	case l.Major != o.Major:
		if l.Major < o.Major {
			return -1
		}
		return 1
	case l.Minor < o.Minor:
		return -1
	case l.Minor > o.Minor:
		return 1
	}
	return 0
}

func (l VersionLine) String() string {
	return strconv.Itoa(l.Major) + "." + strconv.Itoa(l.Minor)
}

// ThisVersionLine is PlatformVersionLine, parsed.
func ThisVersionLine() VersionLine {
	l, _ := ParseVersionLine(PlatformVersionLine)
	return l
}
