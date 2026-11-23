/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
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

// A website on the main address is written only with somebody's
// acknowledgement of the rule for it, and the entry says whose and when: the
// approver's on a publication, the reviewer's on a review. An entry approved
// before the rule was asked carries none; it is read back as it is and stays
// published, and the next review of it is asked like any other.
func TestTheMainAddressEntryRecordsWhoAcknowledgedTheRule(t *testing.T) {
	remote := dt.Remote(t, "demo")
	exposures := strings.TrimSuffix(dt.TenantPath("demo"), "tenant.yaml") + "exposures.yaml"
	// What the director wrote before it asked: apex, and nobody's word.
	dt.Commit(t, remote, map[string]string{exposures: `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  exposures:
    - install: website
      exposureName: site
      owner: u-tom
      reviewAt: ` + time.Now().Add(100*24*time.Hour).UTC().Format(time.RFC3339) + `
      apex: true
      publishedAt: 2026-09-01T00:00:00Z
      lastReviewedBy: u-tom
      lastReviewedAt: 2026-09-01T00:00:00Z
`})
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	meta := func(subject string) gitops.Meta {
		return gitops.Meta{Author: gitops.Person{Name: subject, Email: subject + "@example.com"},
			Subject: subject, RequestID: "req-1", Decision: "can_expose tenant:demo"}
	}
	review := time.Now().Add(200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	site := func() gitops.Exposure {
		t.Helper()
		all, err := g.TenantExposures(ctx, "demo")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range all {
			if e.Install == "website" && e.ExposureName == "site" {
				return e
			}
		}
		t.Fatalf("the website is gone from the registry: %+v", all)
		return gitops.Exposure{}
	}

	// The old entry is read as it is, and something published beside it
	// leaves it alone.
	if _, err := g.PublishExposure(ctx, "demo", gitops.Exposure{
		Install: "nextcloud", ExposureName: "shares", Owner: "u-mia", ReviewAt: review,
		ApexAcknowledgedBy: "u-mia", // not an apex entry: recorded nowhere
	}, meta("u-mia")); err != nil {
		t.Fatal(err)
	}
	if old := site(); !old.Apex || old.Owner != "u-tom" || old.ApexAcknowledgedBy != "" || old.ApexAcknowledgedAt != "" {
		t.Fatalf("the entry approved before the rule was asked = %+v", old)
	}
	if file := dt.RemoteFile(t, remote, exposures); strings.Contains(file, "apexAcknowledged") {
		t.Fatalf("an acknowledgement was written for an entry nobody acknowledged, or for an ordinary surface:\n%s", file)
	}

	// Its review without the word is refused, and nothing is written.
	_, err := g.PublishExposure(ctx, "demo", gitops.Exposure{
		Install: "website", ExposureName: "site", Owner: "u-mia", ReviewAt: review, Apex: true,
	}, meta("u-mia"))
	if !errors.Is(err, gitops.ErrMainAddressNotAcknowledged) {
		t.Fatalf("a review of the website without the acknowledgement: %v", err)
	}
	if old := site(); old.LastReviewedBy != "u-tom" {
		t.Fatalf("the refused review was recorded: %+v", old)
	}

	// With it, the reviewer is who acknowledged; the owner stays.
	if _, err := g.PublishExposure(ctx, "demo", gitops.Exposure{
		Install: "website", ExposureName: "site", Owner: "u-mia", ReviewAt: review, Apex: true,
		ApexAcknowledgedBy: "u-mia",
	}, meta("u-mia")); err != nil {
		t.Fatal(err)
	}
	got := site()
	if got.Owner != "u-tom" || got.ApexAcknowledgedBy != "u-mia" || got.ApexAcknowledgedAt == "" ||
		got.ApexAcknowledgedAt != got.LastReviewedAt {
		t.Fatalf("the reviewed entry = %+v; it must keep its owner and say who acknowledged, when", got)
	}
}

// The cookies of an app's own sign-in are recorded with an approval of that
// kind and only with it, and only as names and paths the registry can write
// line by line; read back they are what was written.
func TestSignInCookiesAreRecordedOnlyWithTheirKind(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	meta := gitops.Meta{Author: gitops.Person{Name: "u-tom", Email: "u-tom@example.com"},
		Subject: "u-tom", RequestID: "req-1", Decision: "can_expose tenant:demo"}
	review := time.Now().Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	entry := func(kind string, c *gentianov1alpha1.SignInCookies) gitops.Exposure {
		return gitops.Exposure{Install: "synapse", ExposureName: "matrix", Owner: "u-tom", ReviewAt: review, Kind: kind, SignInCookies: c}
	}
	good := &gentianov1alpha1.SignInCookies{Names: []string{"oidc_session"}, Paths: []string{"/_synapse/client/oidc"}}
	for what, e := range map[string]gitops.Exposure{
		"cookies on another kind":   entry(gitops.KindPublicAppCredential, good),
		"the kind without cookies":  entry(gitops.KindPublicAppSignIn, nil),
		"a name that is not a name": entry(gitops.KindPublicAppSignIn, &gentianov1alpha1.SignInCookies{Names: []string{"a]\n  evil: x"}, Paths: []string{"/x"}}),
		"a path that is not a path": entry(gitops.KindPublicAppSignIn, &gentianov1alpha1.SignInCookies{Names: []string{"a"}, Paths: []string{"/x, /y"}}),
	} {
		if _, err := g.PublishExposure(ctx, "demo", e, meta); err == nil {
			t.Errorf("%s was recorded", what)
		}
	}
	if _, err := g.PublishExposure(ctx, "demo", entry(gitops.KindPublicAppSignIn, good), meta); err != nil {
		t.Fatal(err)
	}
	all, err := g.TenantExposures(ctx, "demo")
	if err != nil || len(all) != 1 || all[0].SignInCookies == nil ||
		strings.Join(all[0].SignInCookies.Names, ",") != "oidc_session" || strings.Join(all[0].SignInCookies.Paths, ",") != "/_synapse/client/oidc" {
		t.Fatalf("read back = %+v, %v", all, err)
	}
}
