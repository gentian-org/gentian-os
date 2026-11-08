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

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// aliasFixture is a cluster with two tenants, one surface of each published.
func aliasFixture(t *testing.T) (*gitops.GitOps, string, func(string) gitops.Meta) {
	t.Helper()
	remote := dt.Remote(t, "team", "acme")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	meta := func(subject string) gitops.Meta {
		return gitops.Meta{Author: gitops.Person{Name: subject, Email: subject + "@example.com"},
			Subject: subject, RequestID: "req-1", Decision: "can_configure cluster:" + dt.Cluster}
	}
	review := time.Now().Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, tenant := range []string{"team", "acme"} {
		if _, err := g.PublishExposure(context.Background(), tenant, gitops.Exposure{
			Install: "reports", ExposureName: "in", Owner: "u-approver", ReviewAt: review,
		}, meta("u-approver")); err != nil {
			t.Fatal(err)
		}
	}
	return g, remote, meta
}

func aliasOf(t *testing.T, g *gitops.GitOps, tenant string) gitops.Exposure {
	t.Helper()
	all, err := g.TenantExposures(context.Background(), tenant)
	if err != nil || len(all) != 1 {
		t.Fatalf("registry of %s = %v, %v", tenant, all, err)
	}
	return all[0]
}

// The cluster's administrator gives a published surface a name directly
// under the cluster's domain, and the entry records it with who and when.
func TestAPublishedSurfaceIsGivenAnAlias(t *testing.T) {
	g, remote, meta := aliasFixture(t)
	ctx := context.Background()

	res, err := g.SetExposureAlias(ctx, "team", "reports", "in", "Statistics."+dt.KernelDomain, meta("u-admin"))
	if err != nil || !res.Changed {
		t.Fatalf("alias = %+v, %v", res, err)
	}
	got := aliasOf(t, g, "team")
	if got.Alias != "statistics" || got.AliasSetBy != "u-admin" || got.AliasSetAt == "" {
		t.Fatalf("entry = %+v", got)
	}
	// Who published it is not who gave it the name.
	if got.Owner != "u-approver" {
		t.Fatalf("owner = %q", got.Owner)
	}
	file := dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/tenants/team/exposures.yaml")
	if !strings.Contains(file, "      alias: statistics\n") {
		t.Fatalf("the committed file names no alias:\n%s", file)
	}

	// Asked again, nothing is written.
	res, err = g.SetExposureAlias(ctx, "team", "reports", "in", "statistics."+dt.KernelDomain, meta("u-admin"))
	if err != nil || res.Changed {
		t.Fatalf("the same alias again = %+v, %v", res, err)
	}
}

// What is not one name directly under the cluster's domain, or is a name the
// platform keeps, is no alias.
func TestWhatCannotBeAnAlias(t *testing.T) {
	g, _, meta := aliasFixture(t)
	ctx := context.Background()
	for _, host := range []string{
		"statistics.example.org",             // another domain
		dt.KernelDomain,                      // the main address itself
		"a.statistics." + dt.KernelDomain,    // two names
		"statistics.team." + dt.KernelDomain, // the surface's own level
		"-x." + dt.KernelDomain,              // not a name
		"id." + dt.KernelDomain,              // the identity provider
		"mail." + dt.KernelDomain,            // the cluster's mail host
		"desktop." + dt.KernelDomain,         // a name the platform keeps in every tenant
		"platform." + dt.KernelDomain,        // the platform tenant
	} {
		_, err := g.SetExposureAlias(ctx, "team", "reports", "in", host, meta("u-admin"))
		if !errors.Is(err, gitops.ErrAliasInvalid) {
			t.Errorf("%s: %v, want ErrAliasInvalid", host, err)
		}
	}
	if got := aliasOf(t, g, "team"); got.Alias != "" {
		t.Fatalf("a refused alias was written: %+v", got)
	}
}

// A tenant's own address is <name>.<cluster's domain>. An alias cannot take
// it, and a tenant cannot be created under a name an alias holds.
func TestAnAliasAndATenantCannotShareAName(t *testing.T) {
	g, _, meta := aliasFixture(t)
	ctx := context.Background()

	_, err := g.SetExposureAlias(ctx, "team", "reports", "in", "acme."+dt.KernelDomain, meta("u-admin"))
	if !errors.Is(err, gitops.ErrAliasHeld) || !strings.Contains(err.Error(), "tenant acme") {
		t.Fatalf("an alias under a tenant's name = %v", err)
	}

	if _, err := g.SetExposureAlias(ctx, "team", "reports", "in", "statistics."+dt.KernelDomain, meta("u-admin")); err != nil {
		t.Fatal(err)
	}
	_, err = g.CreateTenant(ctx, gitops.NewTenant{Name: "statistics"}, meta("u-admin"))
	if !errors.Is(err, gitops.ErrNameIsAlias) || !strings.Contains(err.Error(), "reports/in of tenant team") {
		t.Fatalf("a tenant under an alias's name = %v", err)
	}
	names, err := g.Tenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name == "statistics" {
			t.Fatal("the refused tenant was committed")
		}
	}

	// With the alias gone the name is free.
	if _, err := g.SetExposureAlias(ctx, "team", "reports", "in", "", meta("u-admin")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "statistics"}, meta("u-admin")); err != nil {
		t.Fatalf("the tenant after the alias was removed: %v", err)
	}
}

// One name is one surface's, whichever tenant it is in.
func TestAnAliasIsHeldByOneSurface(t *testing.T) {
	g, _, meta := aliasFixture(t)
	ctx := context.Background()
	if _, err := g.SetExposureAlias(ctx, "team", "reports", "in", "statistics."+dt.KernelDomain, meta("u-admin")); err != nil {
		t.Fatal(err)
	}
	_, err := g.SetExposureAlias(ctx, "acme", "reports", "in", "statistics."+dt.KernelDomain, meta("u-admin"))
	if !errors.Is(err, gitops.ErrAliasHeld) || !strings.Contains(err.Error(), "reports/in of tenant team") {
		t.Fatalf("a second surface under the alias = %v", err)
	}
	// The holder may take another name, which frees the first.
	if _, err := g.SetExposureAlias(ctx, "team", "reports", "in", "numbers."+dt.KernelDomain, meta("u-admin")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SetExposureAlias(ctx, "acme", "reports", "in", "statistics."+dt.KernelDomain, meta("u-admin")); err != nil {
		t.Fatalf("the freed name: %v", err)
	}
}

// An alias is for what the tenant's approver has published, and nothing is
// published by asking for one.
func TestOnlyAPublishedSurfaceIsGivenAnAlias(t *testing.T) {
	g, _, meta := aliasFixture(t)
	ctx := context.Background()
	host := "statistics." + dt.KernelDomain

	_, err := g.SetExposureAlias(ctx, "team", "reports", "other", host, meta("u-admin"))
	if !errors.Is(err, gitops.ErrNotPublished) {
		t.Fatalf("an entry nobody approved = %v", err)
	}
	if all, _ := g.TenantExposures(ctx, "team"); len(all) != 1 {
		t.Fatalf("asking for an alias published something: %v", all)
	}

	// An approval that has run out is not one.
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, err := g.PublishExposure(ctx, "acme", gitops.Exposure{
		Install: "reports", ExposureName: "in", Owner: "u-approver", ReviewAt: past, ExpiresAt: past,
	}, meta("u-approver")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SetExposureAlias(ctx, "acme", "reports", "in", host, meta("u-admin")); !errors.Is(err, gitops.ErrNotPublished) {
		t.Fatalf("an expired approval = %v", err)
	}

	// An entry behind sign-in has no public address to give a second name.
	review := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := g.PublishExposure(ctx, "team", gitops.Exposure{
		Install: "flows", ExposureName: "web", Kind: gitops.KindSignInAppAuthorization, Owner: "u-approver", ReviewAt: review,
	}, meta("u-approver")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SetExposureAlias(ctx, "team", "flows", "web", host, meta("u-admin")); !errors.Is(err, gitops.ErrNotPublished) {
		t.Fatalf("an entry behind sign-in = %v", err)
	}
}

// The approver's review keeps the alias the administrator gave, and cannot
// state one; withdrawing the surface takes the alias with it.
func TestAnAliasFollowsItsSurface(t *testing.T) {
	g, _, meta := aliasFixture(t)
	ctx := context.Background()
	if _, err := g.SetExposureAlias(ctx, "team", "reports", "in", "statistics."+dt.KernelDomain, meta("u-admin")); err != nil {
		t.Fatal(err)
	}
	before := aliasOf(t, g, "team")

	review := time.Now().Add(200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := g.PublishExposure(ctx, "team", gitops.Exposure{
		Install: "reports", ExposureName: "in", Owner: "u-other", ReviewAt: review, Alias: "id",
	}, meta("u-other")); err != nil {
		t.Fatal(err)
	}
	after := aliasOf(t, g, "team")
	if after.Alias != "statistics" || after.AliasSetBy != "u-admin" || after.AliasSetAt != before.AliasSetAt {
		t.Fatalf("after a review = %+v", after)
	}
	if after.LastReviewedBy != "u-other" {
		t.Fatalf("the review was not recorded: %+v", after)
	}

	// A first publication cannot bring an alias with it either.
	if _, err := g.PublishExposure(ctx, "team", gitops.Exposure{
		Install: "shop", ExposureName: "api", Owner: "u-other", ReviewAt: review, Alias: "shop",
	}, meta("u-other")); err != nil {
		t.Fatal(err)
	}
	all, err := g.TenantExposures(ctx, "team")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if e.Install == "shop" && e.Alias != "" {
			t.Fatalf("an approver stated an alias: %+v", e)
		}
	}

	if _, err := g.WithdrawExposure(ctx, "team", "reports", "in", meta("u-other")); err != nil {
		t.Fatal(err)
	}
	// The name is free again: nothing holds it once its surface is gone.
	if _, err := g.SetExposureAlias(ctx, "acme", "reports", "in", "statistics."+dt.KernelDomain, meta("u-admin")); err != nil {
		t.Fatalf("the name after its surface was withdrawn: %v", err)
	}
}

// Removing an alias a surface does not have changes nothing.
func TestRemovingAnAliasThatIsNotThere(t *testing.T) {
	g, _, meta := aliasFixture(t)
	res, err := g.SetExposureAlias(context.Background(), "team", "reports", "in", "", meta("u-admin"))
	if err != nil || res.Changed {
		t.Fatalf("removing nothing = %+v, %v", res, err)
	}
}
