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
