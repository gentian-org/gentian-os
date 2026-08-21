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

package backup

import (
	"fmt"
	"testing"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// nightly builds one export per day, oldest first, ending the day before now.
func nightly(now time.Time, days int) []Candidate {
	out := make([]Candidate, 0, days)
	for i := days; i >= 1; i-- {
		at := now.AddDate(0, 0, -i)
		out = append(out, Candidate{
			Name:       fmt.Sprintf("export-%s", at.UTC().Format("2006-01-02")),
			FinishedAt: at,
		})
	}
	return out
}

func kept(all []Candidate, doomed []string) map[string]bool {
	dead := make(map[string]bool, len(doomed))
	for _, d := range doomed {
		dead[d] = true
	}
	alive := make(map[string]bool, len(all))
	for _, c := range all {
		if !dead[c.Name] {
			alive[c.Name] = true
		}
	}
	return alive
}

// Retention that deletes by default would be a policy nobody chose.
func TestNoRetentionDeletesNothing(t *testing.T) {
	now := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	all := nightly(now, 30)
	if doomed := SelectForDeletion(all, gentianov1alpha1.BackupRetention{}, now); len(doomed) != 0 {
		t.Fatalf("unset retention deleted %d export(s)", len(doomed))
	}
}

func TestKeepLastRetainsTheMostRecent(t *testing.T) {
	now := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	all := nightly(now, 10)
	doomed := SelectForDeletion(all, gentianov1alpha1.BackupRetention{KeepLast: 3}, now)
	alive := kept(all, doomed)

	if len(alive) != 3 {
		t.Fatalf("kept %d, want 3", len(alive))
	}
	for _, want := range []string{"export-2026-08-20", "export-2026-08-19", "export-2026-08-18"} {
		if !alive[want] {
			t.Errorf("%s was deleted; keepLast must retain the most recent", want)
		}
	}
}

// The point of the tiers: a year of history for a handful of bundles rather
// than 365. Keeping the last N alone reaches back only N nights, and a mistake
// noticed on night N+1 is unrecoverable.
func TestTiersReachBackFurtherThanKeepLast(t *testing.T) {
	now := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	all := nightly(now, 400)

	doomed := SelectForDeletion(all, gentianov1alpha1.BackupRetention{
		KeepLast: 3, KeepWeekly: 4, KeepMonthly: 6, KeepYearly: 2,
	}, now)
	alive := kept(all, doomed)

	// Far fewer than a year of nightlies, but reaching back beyond a year.
	if len(alive) > 20 {
		t.Errorf("kept %d exports; the tiers should be far sparser", len(alive))
	}
	var oldest time.Time
	for _, c := range all {
		if alive[c.Name] && (oldest.IsZero() || c.FinishedAt.Before(oldest)) {
			oldest = c.FinishedAt
		}
	}
	// Each tier keeps the LATEST export within a period, so keepYearly: 2
	// reaches back to last December rather than to this date a year ago. That
	// is still two orders of magnitude beyond keepLast: 3, which is the claim.
	if now.Sub(oldest) < 180*24*time.Hour {
		t.Errorf("oldest surviving export is only %v old; the tiers did not reach back",
			now.Sub(oldest).Round(24*time.Hour))
	}
	if now.Sub(oldest) <= 3*24*time.Hour {
		t.Error("the tiers reached no further than keepLast alone")
	}
}

// The tiers are a union, not a sequence: a bundle any rule keeps survives.
func TestTiersAreAUnion(t *testing.T) {
	now := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	all := nightly(now, 40)

	onlyWeekly := kept(all, SelectForDeletion(all,
		gentianov1alpha1.BackupRetention{KeepWeekly: 3}, now))
	both := kept(all, SelectForDeletion(all,
		gentianov1alpha1.BackupRetention{KeepLast: 5, KeepWeekly: 3}, now))

	for name := range onlyWeekly {
		if !both[name] {
			t.Errorf("%s survived weekly alone but not weekly plus keepLast; tiers must union", name)
		}
	}
	if len(both) <= len(onlyWeekly) {
		t.Errorf("adding keepLast kept %d, no more than weekly alone (%d)", len(both), len(onlyWeekly))
	}
}

// A tenant that backs up rarely must not lose history to periods that contain
// nothing: the tiers count exports, not calendar slots.
func TestSparseHistoryKeepsWhatExists(t *testing.T) {
	now := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	all := []Candidate{
		{Name: "old", FinishedAt: now.AddDate(-3, 0, 0)},
		{Name: "middle", FinishedAt: now.AddDate(-2, 0, 0)},
		{Name: "recent", FinishedAt: now.AddDate(-1, 0, 0)},
	}
	doomed := SelectForDeletion(all, gentianov1alpha1.BackupRetention{KeepYearly: 3}, now)
	if len(doomed) != 0 {
		t.Fatalf("deleted %v; three exports in three distinct years all fit keepYearly: 3", doomed)
	}
}

// An export finishing after the sweep's clock is not evidence about any
// period; counting it would let a clock skew evict a real bundle.
func TestFutureExportsDoNotConsumeATier(t *testing.T) {
	now := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	all := []Candidate{
		{Name: "future", FinishedAt: now.Add(48 * time.Hour)},
		{Name: "yesterday", FinishedAt: now.AddDate(0, 0, -1)},
		{Name: "older", FinishedAt: now.AddDate(0, 0, -2)},
	}
	alive := kept(all, SelectForDeletion(all, gentianov1alpha1.BackupRetention{KeepDaily: 1}, now))
	if !alive["yesterday"] {
		t.Error("a future-dated export consumed the daily tier and evicted a real one")
	}
}
