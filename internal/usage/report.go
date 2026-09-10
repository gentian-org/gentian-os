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
	"context"
	"time"
)

// PlanInterval is one continuous stretch during which a tenant was on one plan.
//
// This is the unit a bill is made of. "The tenant was on one node for the first
// seventeen days and two for the rest" is a statement about intervals, and a
// platform that stores only a current plan cannot make it.
type PlanInterval struct {
	Plan       string    `json:"plan"`
	ProductSku string    `json:"productSku,omitempty"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	// Seconds is To-From, precomputed so a consumer that prices by time does
	// not have to agree with this code about how to subtract two timestamps
	// across a daylight-saving boundary.
	Seconds int64 `json:"seconds"`
	// Partial marks an interval clipped by the window rather than ended by a
	// plan change — the first and usually the last in any report. A biller
	// pro-rating a month needs to know which ends are real.
	Partial bool `json:"partial"`
}

// Report is a tenant's billable plan history over a window.
type Report struct {
	Tenant    string         `json:"tenant"`
	From      time.Time      `json:"from"`
	To        time.Time      `json:"to"`
	Intervals []PlanInterval `json:"intervals"`
	// Incomplete is true when nothing is known about the plan in force at the
	// start of the window — no event before it and no sample within it. The
	// report is then a record of what changed, not of what was billed, and
	// saying so is better than emitting an interval built on a guess.
	Incomplete bool `json:"incomplete"`
}

// BuildReport resolves a window into plan intervals.
//
// The plan at the start of the window comes from the last event before it. When
// there is none — a tenant whose plan predates the feature, or one that has
// never changed plan — the earliest sample inside the window supplies it
// instead, because a sample carries the plan it was taken under. Only when
// neither exists is the window reported as incomplete.
func BuildReport(ctx context.Context, store *Store, tenant string, from, to time.Time) (*Report, error) {
	report := &Report{Tenant: tenant, From: from.UTC(), To: to.UTC()}
	if !report.To.After(report.From) {
		return report, nil
	}

	events, err := store.PlanEvents(ctx, report.From, report.To)
	if err != nil {
		return nil, err
	}

	current, currentSku, known := "", "", false
	if prior, ok, err := store.LastPlanBefore(ctx, report.From); err != nil {
		return nil, err
	} else if ok {
		current, currentSku, known = prior.ToPlan, prior.ProductSku, true
	}

	if !known {
		// One sample is enough and the cheapest possible query for it is a
		// one-bucket thinning of the whole window, which returns the newest
		// row in it. That is not the earliest — but any sample inside a window
		// with no plan change before its own timestamp names the plan the
		// window opened on, because a change would have produced an event.
		samples, err := store.Samples(ctx, report.From, report.To, report.To.Sub(report.From))
		if err != nil {
			return nil, err
		}
		if len(samples) > 0 && samples[0].Plan != "" && len(events) == 0 {
			current, currentSku, known = samples[0].Plan, samples[0].ProductSku, true
		}
	}

	report.Intervals, report.Incomplete = buildIntervals(
		events, current, currentSku, known, report.From, report.To)
	return report, nil
}

// buildIntervals turns a plan history into the stretches a bill is made of.
//
// Separated from the queries so the arithmetic that decides what a tenant owes
// can be tested without a database. Every branch here is a case that showed up
// on paper: a window with no events, one opening mid-plan, one whose opening
// plan is only knowable from the first event's fromPlan, and one about which
// nothing is known at all.
func buildIntervals(
	events []PlanEvent,
	current, currentSku string,
	known bool,
	from, to time.Time,
) ([]PlanInterval, bool) {
	var intervals []PlanInterval
	cursor := from

	for _, event := range events {
		at := event.OccurredAt.UTC()
		if at.Before(cursor) || at.After(to) {
			continue
		}
		switch {
		case known && at.After(cursor):
			intervals = append(intervals,
				interval(current, currentSku, cursor, at, cursor.Equal(from)))
		case !known && event.FromPlan != "" && at.After(cursor):
			// The event names what it moved away from, which retroactively
			// answers what the window opened on. The SKU is not recoverable
			// this way — the event carries the SKU of the plan it moved *to* —
			// so the interval is left without one rather than given the wrong
			// one, and a biller sees a gap instead of a mispriced stretch.
			intervals = append(intervals, interval(event.FromPlan, "", cursor, at, true))
		}
		current, currentSku, known = event.ToPlan, event.ProductSku, true
		cursor = at
	}

	if known && to.After(cursor) {
		intervals = append(intervals, interval(current, currentSku, cursor, to, true))
	}
	return intervals, !known
}

func interval(plan, sku string, from, to time.Time, partial bool) PlanInterval {
	return PlanInterval{
		Plan:       plan,
		ProductSku: sku,
		From:       from,
		To:         to,
		Seconds:    int64(to.Sub(from).Seconds()),
		Partial:    partial,
	}
}
