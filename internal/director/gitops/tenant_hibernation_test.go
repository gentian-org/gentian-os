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

func listed(t *testing.T, g *gitops.GitOps, name string) gitops.Tenant {
	t.Helper()
	tenants, err := g.TenantDetails(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range tenants {
		if tenant.Name == name {
			return tenant
		}
	}
	t.Fatalf("no tenant %s in the listing", name)
	return gitops.Tenant{}
}

// A tenant's sleep is one block of its manifest. Putting it to sleep, keeping
// it awake until a time and waking it each replace the block whole; the state
// the manifest already declares commits nothing; and the listing says what
// holds now.
func TestATenantsSleepIsOneBlockOfItsManifest(t *testing.T) {
	remote := dt.Remote(t, "platform", "acme")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	path := "clusters/" + dt.Cluster + "/tenants/acme/tenant.yaml"
	head := func() string { return dt.Git(t, "", "--git-dir", remote, "rev-parse", "main") }

	// Awake, and asked to wake: nothing to write.
	if res, err := g.SetTenantHibernation(ctx, "acme", gitops.Hibernation{}, tenantMeta()); err != nil || res.Changed {
		t.Fatalf("waking a tenant that is awake must not commit: %+v, %v", res, err)
	}
	if strings.Contains(dt.RemoteFile(t, remote, path), "hibernation") {
		t.Fatal("a wake of an awake tenant wrote the block")
	}

	res, err := g.SetTenantHibernation(ctx, "acme", gitops.Hibernation{Asleep: true}, tenantMeta())
	if err != nil || !res.Changed {
		t.Fatalf("sleep: %+v, %v", res, err)
	}
	if doc := dt.RemoteFile(t, remote, path); !strings.Contains(doc, "  hibernation:\n    asleep: true\n    setBy: ") {
		t.Fatalf("the block does not say who asked:\n%s", doc)
	}
	if got := listed(t, g, "acme"); !got.Asleep || got.AwakeUntil != nil {
		t.Fatalf("the listing says %+v", got)
	}
	if res, err := g.SetTenantHibernation(ctx, "acme", gitops.Hibernation{Asleep: true}, tenantMeta()); err != nil || res.Changed {
		t.Fatalf("the same state again must not commit: %+v, %v", res, err)
	}

	until := time.Now().Add(72 * time.Hour)
	if res, err := g.SetTenantHibernation(ctx, "acme", gitops.Hibernation{Asleep: true, AwakeUntil: &until}, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("awake until: %+v, %v", res, err)
	}
	stamp := until.UTC().Truncate(time.Second).Format(time.RFC3339)
	if doc := dt.RemoteFile(t, remote, path); !strings.Contains(doc, "  hibernation:\n    asleep: true\n    awakeUntil: \""+stamp+"\"") {
		t.Fatalf("tenant.yaml:\n%s", doc)
	}
	if got := listed(t, g, "acme"); got.Asleep || got.AwakeUntil == nil || got.AwakeUntil.UTC().Format(time.RFC3339) != stamp {
		t.Fatalf("the listing says %+v", got)
	}
	if res, err := g.SetTenantHibernation(ctx, "acme", gitops.Hibernation{Asleep: true, AwakeUntil: &until}, tenantMeta()); err != nil || res.Changed {
		t.Fatalf("the same time again must not commit: %+v, %v", res, err)
	}

	// A time that has passed: the tenant sleeps again without a commit.
	past := time.Now().Add(-time.Hour)
	if _, err := g.SetTenantHibernation(ctx, "acme", gitops.Hibernation{Asleep: true, AwakeUntil: &past}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if got := listed(t, g, "acme"); !got.Asleep || got.AwakeUntil != nil {
		t.Fatalf("past its time the listing says %+v", got)
	}

	if res, err := g.SetTenantHibernation(ctx, "acme", gitops.Hibernation{}, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("wake: %+v, %v", res, err)
	}
	doc := dt.RemoteFile(t, remote, path)
	if !strings.Contains(doc, "  hibernation:\n    asleep: false") || strings.Contains(doc, "awakeUntil") {
		t.Fatalf("the block was not replaced whole:\n%s", doc)
	}
	if got := listed(t, g, "acme"); got.Asleep || got.AwakeUntil != nil {
		t.Fatalf("the listing says %+v", got)
	}

	before := head()
	if _, err := g.SetTenantHibernation(ctx, "platform", gitops.Hibernation{Asleep: true}, tenantMeta()); !errors.Is(err, gitops.ErrPlatformTenantSleeps) {
		t.Fatalf("the platform tenant was not refused: %v", err)
	}
	if head() != before {
		t.Fatal("a refused sleep was committed")
	}
}
