/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usage

import (
	"testing"
	"time"
)

func at(day int) time.Time {
	return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC)
}

// The case the whole feature exists for: a month in which the tenant upgraded
// partway through resolves to two priced stretches, not one current plan.
func TestBuildIntervalsSplitsAMonthAtThePlanChange(t *testing.T) {
	events := []PlanEvent{
		{OccurredAt: at(18), FromPlan: "base-plus-8", ToPlan: "base-plus-16", ProductSku: "sku-16"},
	}
	intervals, incomplete := buildIntervals(events, "base-plus-8", "sku-8", true, at(1), at(31))

	if incomplete {
		t.Fatal("a window with a known opening plan is not incomplete")
	}
	if len(intervals) != 2 {
		t.Fatalf("expected two intervals, got %d: %+v", len(intervals), intervals)
	}
	if intervals[0].Plan != "base-plus-8" || intervals[0].ProductSku != "sku-8" {
		t.Errorf("first interval wrong: %+v", intervals[0])
	}
	if intervals[0].Seconds != int64((17 * 24 * time.Hour).Seconds()) {
		t.Errorf("expected 17 days on the first plan, got %d seconds", intervals[0].Seconds)
	}
	if intervals[1].Plan != "base-plus-16" || intervals[1].ProductSku != "sku-16" {
		t.Errorf("second interval wrong: %+v", intervals[1])
	}
	if intervals[1].Seconds != int64((13 * 24 * time.Hour).Seconds()) {
		t.Errorf("expected 13 days on the second plan, got %d seconds", intervals[1].Seconds)
	}
}

func TestBuildIntervalsCoversAQuietWindowWithOneInterval(t *testing.T) {
	intervals, incomplete := buildIntervals(nil, "base", "sku-base", true, at(1), at(31))
	if incomplete {
		t.Fatal("a known plan and no changes is a complete window")
	}
	if len(intervals) != 1 || intervals[0].Plan != "base" {
		t.Fatalf("expected one base interval, got %+v", intervals)
	}
	if !intervals[0].Partial {
		t.Error("an interval clipped by the window at both ends is partial")
	}
}

// With no prior event the first change still says what it moved away from, and
// that retroactively names the plan the window opened on.
func TestBuildIntervalsRecoversTheOpeningPlanFromTheFirstEvent(t *testing.T) {
	events := []PlanEvent{
		{OccurredAt: at(10), FromPlan: "base", ToPlan: "base-plus-8", ProductSku: "sku-8"},
	}
	intervals, incomplete := buildIntervals(events, "", "", false, at(1), at(31))

	if incomplete {
		t.Fatal("the first event's fromPlan makes the window knowable")
	}
	if len(intervals) != 2 {
		t.Fatalf("expected two intervals, got %+v", intervals)
	}
	if intervals[0].Plan != "base" {
		t.Errorf("expected the opening plan to be recovered, got %q", intervals[0].Plan)
	}
	// The event carries the SKU of the plan it moved *to*, so the recovered
	// opening interval must not borrow it — a gap is honest, a wrong price is not.
	if intervals[0].ProductSku != "" {
		t.Errorf("the recovered interval must not carry the new plan's SKU, got %q", intervals[0].ProductSku)
	}
}

func TestBuildIntervalsReportsAWindowItKnowsNothingAbout(t *testing.T) {
	intervals, incomplete := buildIntervals(nil, "", "", false, at(1), at(31))
	if !incomplete {
		t.Fatal("a window with no prior plan and no events is incomplete")
	}
	if len(intervals) != 0 {
		t.Fatalf("expected no invented intervals, got %+v", intervals)
	}
}

func TestBuildIntervalsIgnoresEventsOutsideTheWindow(t *testing.T) {
	events := []PlanEvent{
		{OccurredAt: at(40), FromPlan: "base", ToPlan: "base-plus-8"},
	}
	intervals, _ := buildIntervals(events, "base", "sku-base", true, at(1), at(31))
	if len(intervals) != 1 || intervals[0].Plan != "base" {
		t.Fatalf("an event after the window must not split it: %+v", intervals)
	}
}

func TestBuildIntervalsHandlesSeveralChangesInOneWindow(t *testing.T) {
	events := []PlanEvent{
		{OccurredAt: at(10), FromPlan: "base", ToPlan: "base-plus-8", ProductSku: "sku-8"},
		{OccurredAt: at(20), FromPlan: "base-plus-8", ToPlan: "base-plus-16", ProductSku: "sku-16"},
	}
	intervals, _ := buildIntervals(events, "base", "sku-base", true, at(1), at(31))
	if len(intervals) != 3 {
		t.Fatalf("expected three intervals, got %+v", intervals)
	}
	want := []string{"base", "base-plus-8", "base-plus-16"}
	for i, plan := range want {
		if intervals[i].Plan != plan {
			t.Errorf("interval %d: expected %s, got %s", i, plan, intervals[i].Plan)
		}
	}
}

// portal-shell-<tenant> holds a SQLAlchemy URL because the portal is what
// normally reads it; pgx rejects the compound scheme.
func TestNormalizeDSNStripsTheDialectSuffix(t *testing.T) {
	got := normalizeDSN("postgresql+psycopg://user:pw@host:5432/demo_shell")
	want := "postgresql://user:pw@host:5432/demo_shell"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestNormalizeDSNLeavesAPlainURLAlone(t *testing.T) {
	in := "postgresql://user:pw@host:5432/demo_shell"
	if got := normalizeDSN(in); got != in {
		t.Fatalf("expected %q unchanged, got %q", in, got)
	}
}

// A password containing '+' must not be mistaken for a dialect suffix: only the
// scheme, ahead of "://", is examined.
func TestNormalizeDSNIgnoresPlusSignsInTheCredentials(t *testing.T) {
	in := "postgresql://user:p+w@host:5432/demo_shell"
	if got := normalizeDSN(in); got != in {
		t.Fatalf("expected %q unchanged, got %q", in, got)
	}
}

// A tenant that slept from day 5 to day 20 of a month on one plan: three
// stretches of that plan, the middle one slept through, cut exactly where it
// fell asleep and where it woke.
func TestAMonthIsSplitWhereTheTenantSleptAndWoke(t *testing.T) {
	plan, _ := buildIntervals(nil, "base", "sku-base", true, at(1), at(31))
	changes := []HibernationEvent{
		{OccurredAt: at(5), Asleep: true, Actor: "tom@example.com"},
		{OccurredAt: at(20), Asleep: false, Actor: "tom@example.com"},
	}
	got := splitAtSleep(plan, changes, false, at(1), at(31))

	want := []struct {
		from, to int
		asleep   bool
		partial  bool
	}{
		{1, 5, false, true},
		{5, 20, true, false},
		{20, 31, false, true},
	}
	if len(got) != len(want) {
		t.Fatalf("expected three intervals, got %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.Plan != "base" || g.ProductSku != "sku-base" {
			t.Errorf("interval %d left its plan: %+v", i, g)
		}
		if !g.From.Equal(at(w.from)) || !g.To.Equal(at(w.to)) || g.Seconds != int64(at(w.to).Sub(at(w.from)).Seconds()) {
			t.Errorf("interval %d is %s to %s (%d s), want day %d to day %d", i, g.From, g.To, g.Seconds, w.from, w.to)
		}
		if g.Asleep != w.asleep || g.Partial != w.partial {
			t.Errorf("interval %d: asleep=%t partial=%t, want %t and %t", i, g.Asleep, g.Partial, w.asleep, w.partial)
		}
	}
}

// Sleep that began before the window, a plan change during it, and an event
// that repeats the state held: the window opens asleep, the plan change cuts
// once, and the repeated event cuts nothing.
func TestSleepIsCarriedIntoTheWindowAndAcrossAPlanChange(t *testing.T) {
	events := []PlanEvent{{OccurredAt: at(10), FromPlan: "base", ToPlan: "base-plus-8", ProductSku: "sku-8"}}
	plan, _ := buildIntervals(events, "base", "sku-base", true, at(1), at(31))
	changes := []HibernationEvent{
		{OccurredAt: at(4), Asleep: true},
		{OccurredAt: at(15), Asleep: false},
	}
	got := splitAtSleep(plan, changes, true, at(1), at(31))

	if len(got) != 3 {
		t.Fatalf("expected three intervals, got %+v", got)
	}
	if !got[0].Asleep || got[0].Plan != "base" || !got[0].To.Equal(at(10)) || !got[0].Partial {
		t.Errorf("first interval: %+v", got[0])
	}
	if !got[1].Asleep || got[1].Plan != "base-plus-8" || !got[1].From.Equal(at(10)) || !got[1].To.Equal(at(15)) || got[1].Partial {
		t.Errorf("second interval: %+v", got[1])
	}
	if got[2].Asleep || got[2].Plan != "base-plus-8" || !got[2].From.Equal(at(15)) || !got[2].Partial {
		t.Errorf("third interval: %+v", got[2])
	}
}

func TestAWindowWithoutSleepIsLeftAsItWas(t *testing.T) {
	plan, _ := buildIntervals(nil, "base", "sku-base", true, at(1), at(31))
	got := splitAtSleep(plan, nil, false, at(1), at(31))
	if len(got) != 1 || got[0].Asleep || !got[0].Partial || got[0].Seconds != plan[0].Seconds {
		t.Fatalf("got %+v", got)
	}
}
