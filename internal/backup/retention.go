/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"fmt"
	"sort"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Candidate is one finished export a sweep may keep or delete.
type Candidate struct {
	Name       string
	FinishedAt time.Time
}

// SelectForDeletion decides which bundles a sweep removes.
//
// Keeping the last N answers "how far back can I go" only in nights: at seven
// nightly bundles the answer is a week, and a mistake noticed on the eighth
// day is unrecoverable. The tiers spread what survives across widening
// intervals — the most recent export of each of the last N days, weeks, months
// and years — so a month of history costs a handful of bundles rather than
// thirty.
//
// The tiers are a union: a bundle kept by any rule survives. That is what
// makes them composable, and why keepLast can stay small while history
// reaches back years.
//
// With nothing set, nothing is deleted. Retention that defaults to deleting
// would be a policy nobody chose.
func SelectForDeletion(
	candidates []Candidate,
	r gentianov1alpha1.BackupRetention,
	now time.Time,
) []string {
	if !r.IsSet() || len(candidates) == 0 {
		return nil
	}

	// Newest first, so "the most recent within a period" is simply the first
	// one seen for that period.
	ordered := append([]Candidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].FinishedAt.After(ordered[j].FinishedAt)
	})

	keep := make(map[string]struct{}, len(ordered))
	for i, c := range ordered {
		if int32(i) < r.KeepLast {
			keep[c.Name] = struct{}{}
		}
	}

	// Each tier walks the same newest-first list and keeps the first export it
	// sees in each distinct period, counting only periods that actually
	// contain one. A tenant that backed up twice in a year still gets two
	// yearly bundles rather than one and a gap.
	tier := func(count int32, bucket func(time.Time) string) {
		if count <= 0 {
			return
		}
		seen := make(map[string]struct{}, count)
		for _, c := range ordered {
			if c.FinishedAt.After(now) {
				continue
			}
			key := bucket(c.FinishedAt)
			if _, ok := seen[key]; ok {
				continue
			}
			if int32(len(seen)) >= count {
				break
			}
			seen[key] = struct{}{}
			keep[c.Name] = struct{}{}
		}
	}

	tier(r.KeepDaily, func(t time.Time) string { return t.UTC().Format("2006-01-02") })
	tier(r.KeepWeekly, func(t time.Time) string {
		year, week := t.UTC().ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	})
	tier(r.KeepMonthly, func(t time.Time) string { return t.UTC().Format("2006-01") })
	tier(r.KeepYearly, func(t time.Time) string { return t.UTC().Format("2006") })

	var doomed []string
	for _, c := range ordered {
		if _, ok := keep[c.Name]; !ok {
			doomed = append(doomed, c.Name)
		}
	}
	return doomed
}
