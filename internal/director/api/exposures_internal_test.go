/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
