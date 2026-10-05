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

package gitops_test

import (
	"context"
	"testing"
	"time"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Looking again at something already published is a review, not a new
// publication: who published it and when stay what they were, and whoever
// looked is recorded as the last reviewer. It may be the same person every
// time -- the point is that somebody looks, not that somebody else does.
func TestReviewingAPublishedSurfaceKeepsWhoPublishedIt(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	meta := func(subject string) gitops.Meta {
		return gitops.Meta{Author: gitops.Person{Name: subject, Email: subject + "@example.com"},
			Subject: subject, RequestID: "req-1", Decision: "can_expose tenant:demo"}
	}
	in := func(days int) string {
		return time.Now().Add(time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	read := func() gitops.Exposure {
		t.Helper()
		all, err := g.TenantExposures(ctx, "demo")
		if err != nil || len(all) != 1 {
			t.Fatalf("registry = %v, %v", all, err)
		}
		return all[0]
	}

	if _, err := g.PublishExposure(ctx, "demo", gitops.Exposure{
		Install: "nextcloud", ExposureName: "shares", Owner: "u-tom", ReviewAt: in(100), Reason: "shared calendars",
	}, meta("u-tom")); err != nil {
		t.Fatal(err)
	}
	first := read()
	// Publishing is the first review.
	if first.Owner != "u-tom" || first.PublishedAt == "" || first.LastReviewedBy != "u-tom" || first.LastReviewedAt == "" {
		t.Fatalf("published = %+v", first)
	}

	// Somebody else looks at it later and gives it another year.
	if _, err := g.PublishExposure(ctx, "demo", gitops.Exposure{
		Install: "nextcloud", ExposureName: "shares", Owner: "u-mia", ReviewAt: in(365), Reason: "still shared",
	}, meta("u-mia")); err != nil {
		t.Fatal(err)
	}
	reviewed := read()
	if reviewed.Owner != "u-tom" || reviewed.PublishedAt != first.PublishedAt {
		t.Fatalf("a review changed who published it or when: %+v", reviewed)
	}
	if reviewed.LastReviewedBy != "u-mia" || reviewed.ReviewAt == first.ReviewAt || reviewed.Reason != "still shared" {
		t.Fatalf("the review was not recorded: %+v", reviewed)
	}
}
