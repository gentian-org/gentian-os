/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"os"
	"regexp"
	"testing"
)

func TestAVersionLineIsMajorAndMinor(t *testing.T) {
	for in, want := range map[string]VersionLine{
		"0.5": {0, 5}, "v0.5": {0, 5}, "0.5.2": {0, 5}, "0.5.2+abc": {0, 5}, "1.12.0-rc1": {1, 12},
	} {
		if got, ok := ParseVersionLine(in); !ok || got != want {
			t.Errorf("%q = %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "5", "0.x", "abc", "0.5.2.1", "0..5", "-1.2"} {
		if got, ok := ParseVersionLine(in); ok {
			t.Errorf("%q read as %v", in, got)
		}
	}
	older, _ := ParseVersionLine("0.4")
	newer, _ := ParseVersionLine("1.0")
	this := ThisVersionLine()
	if older.Compare(this) != -1 || newer.Compare(this) != 1 || this.Compare(this) != 0 {
		t.Errorf("compare: %d %d %d", older.Compare(this), newer.Compare(this), this.Compare(this))
	}
}

// The line is never behind the chart's: a release bumps the chart, and a
// platform that called itself older than its chart would read its own
// bundles as another line's.
func TestThePlatformVersionLineIsNotBehindTheChart(t *testing.T) {
	raw, err := os.ReadFile("../../charts/gentian-os/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^version:\s*"?([0-9.]+)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("the chart states no version")
	}
	chart, ok := ParseVersionLine(string(m[1]))
	if !ok {
		t.Fatalf("the chart's version %q has no line", m[1])
	}
	if ThisVersionLine().Compare(chart) < 0 {
		t.Errorf("PlatformVersionLine %s is behind the chart's %s", PlatformVersionLine, chart)
	}
}
