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

package api

import (
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// An overdue review is reported and takes nothing down; an expiry that has
// passed does.
func TestAnOverdueReviewIsReportedAndNothingIsTakenDown(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	live, expired, due := sortExposures([]gitops.Exposure{
		{ExposureName: "site", ReviewAt: at(-48 * time.Hour)},
		{ExposureName: "fresh", ReviewAt: at(48 * time.Hour)},
		{ExposureName: "ended", ReviewAt: at(-72 * time.Hour), ExpiresAt: at(-24 * time.Hour)},
	}, now)
	if len(live) != 2 || len(expired) != 1 || expired[0].ExposureName != "ended" {
		t.Fatalf("live=%v expired=%v", live, expired)
	}
	if len(due) != 1 || due[0].ExposureName != "site" {
		t.Fatalf("reviewDue=%v", due)
	}
}
