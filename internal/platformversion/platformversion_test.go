/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package platformversion

import "testing"

func TestAFloorIsALineAndNothingFiner(t *testing.T) {
	for _, in := range []string{">=0.5", "0.5", "v0.5", ">=v0.5", " >=0.5 "} {
		f, err := ParseFloor(in)
		if err != nil || !f.Stated || f.Min != (Version{0, 5}) || f.String() != ">=0.5" {
			t.Errorf("ParseFloor(%q) = %+v, %v", in, f, err)
		}
	}
	for _, in := range []string{">=0.5.1", "0", "a.b", ">0.5", "<=0.5", "0.05", "0.5-dev", ">= 0.5"} {
		if _, err := ParseFloor(in); err == nil {
			t.Errorf("ParseFloor(%q) accepted", in)
		}
	}
	f, err := ParseFloor("")
	if err != nil || f.Stated || f.String() != "" {
		t.Errorf("ParseFloor(\"\") = %+v, %v", f, err)
	}
}

func TestAFloorAdmitsItsLineAndEveryLaterOne(t *testing.T) {
	f, _ := ParseFloor(">=0.5")
	for v, want := range map[Version]bool{{0, 4}: false, {0, 5}: true, {0, 6}: true, {1, 0}: true} {
		if got := f.Admits(v); got != want {
			t.Errorf("%s admits %s = %v, want %v", f, v, got, want)
		}
	}
	if !(Floor{}).Admits(Version{0, 0}) {
		t.Error("an unstated floor refused 0.0")
	}
}

func TestUnmetSaysWhatIsNeededAndWhatThisIs(t *testing.T) {
	if got := Unmet(""); got != "" {
		t.Errorf("Unmet(\"\") = %q", got)
	}
	if got := Unmet(">=" + Line); got != "" {
		t.Errorf("this line is unmet: %q", got)
	}
	if got := Unmet(">=99.0"); got != "needs platform 99.0; this cluster is "+Line {
		t.Errorf("Unmet = %q", got)
	}
	if got := Unmet(">=0.5.1"); got == "" {
		t.Error("an unreadable floor was admitted")
	}
}

func TestParseReadsALineOutOfAnyVersion(t *testing.T) {
	for in, want := range map[string]Version{"0.5": {0, 5}, "v0.5": {0, 5}, "0.5.3": {0, 5}, "0.5.0-dev.37+g1": {0, 5}} {
		if got, ok := Parse(in); !ok || got != want {
			t.Errorf("Parse(%q) = %v, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "5", "a.b", "0.5.1.2"} {
		if _, ok := Parse(in); ok {
			t.Errorf("Parse(%q) accepted", in)
		}
	}
}
