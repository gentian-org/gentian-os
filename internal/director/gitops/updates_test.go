/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"testing"
	"time"
)

func TestAWindowIsOpenBetweenItsTimesAndMayCrossMidnight(t *testing.T) {
	day := func(h, m int) time.Time { return time.Date(2026, 10, 19, h, m, 0, 0, time.UTC) }
	w := &UpdateWindow{Start: "02:00", End: "05:00"}
	for at, want := range map[time.Time]bool{day(1, 59): false, day(2, 0): true, day(4, 59): true, day(5, 0): false} {
		if got := w.Open(at); got != want {
			t.Errorf("%s open at %s = %v, want %v", w.Start+"-"+w.End, at.Format("15:04"), got, want)
		}
	}
	night := &UpdateWindow{Start: "22:00", End: "03:00", Timezone: "Europe/Zurich"}
	// 23:00 Zurich in October is 21:00 UTC.
	if !night.Open(day(21, 0)) || night.Open(day(12, 0)) {
		t.Errorf("a window across midnight in Zurich: open at 21:00Z = %v, at 12:00Z = %v", night.Open(day(21, 0)), night.Open(day(12, 0)))
	}
	var none *UpdateWindow
	if !none.Open(day(12, 0)) || !(&UpdateWindow{Start: "02:00"}).Open(day(12, 0)) {
		t.Error("a window with no bounds is always open")
	}
}

func TestTheModeForAnAppFollowsTheTenantThenTheAppThenTheCluster(t *testing.T) {
	p := UpdatePolicy{Mode: UpdateManual, Apps: map[string]UpdateMode{"wiki": UpdateTenant}}
	if got := p.ModeFor("wiki", ""); got != UpdateTenant {
		t.Errorf("app setting: %s", got)
	}
	if got := p.ModeFor("wiki", "latest"); got != UpdateLatest {
		t.Errorf("tenant override: %s", got)
	}
	if got := p.ModeFor("notes", ""); got != UpdateManual {
		t.Errorf("cluster mode: %s", got)
	}
	if got := (UpdatePolicy{}).ModeFor("notes", "bogus"); got != UpdateLatest {
		t.Errorf("no policy and a bad override: %s", got)
	}
}
