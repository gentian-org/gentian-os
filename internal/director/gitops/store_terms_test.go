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

	"github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// The App Store's terms, accepted: the version and the time, two quoted
// lines of the tenant's manifest. The same version again commits nothing
// and keeps the time; a new version replaces both; a version the Tenant
// kind would refuse is refused before git.
func TestAcceptingTheStoreTermsIsTwoLinesOfTheManifest(t *testing.T) {
	remote := directortest.Remote(t, "acme")
	g := gitops.NewGitOps(directortest.Clone(t, remote), remote, directortest.Cluster, director)
	ctx := context.Background()
	path := "clusters/" + directortest.Cluster + "/tenants/acme/tenant.yaml"

	if got, err := g.TenantStoreTerms(ctx, "acme"); err != nil || got != nil {
		t.Fatalf("before: %+v, %v", got, err)
	}
	first := time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC)
	res, err := g.SetTenantStoreTerms(ctx, "acme", "1.10", first, tenantMeta())
	if err != nil || !res.Changed {
		t.Fatalf("accept: %+v, %v", res, err)
	}
	doc := directortest.RemoteFile(t, remote, path)
	if !strings.Contains(doc, "  storeTerms:\n    version: \"1.10\"\n    acceptedAt: \"2026-10-25T08:00:00Z\"") {
		t.Fatalf("tenant.yaml:\n%s", doc)
	}
	got, err := g.TenantStoreTerms(ctx, "acme")
	if err != nil || got == nil || got.Version != "1.10" || got.AcceptedAt != "2026-10-25T08:00:00Z" {
		t.Fatalf("read back: %+v, %v", got, err)
	}

	if res, err := g.SetTenantStoreTerms(ctx, "acme", "1.10", first.Add(time.Hour), tenantMeta()); err != nil || res.Changed {
		t.Fatalf("the same version again must not commit: %+v, %v", res, err)
	}
	if res, err := g.SetTenantStoreTerms(ctx, "acme", "2027-01", first.Add(time.Hour), tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("a new version: %+v, %v", res, err)
	}
	if got, _ := g.TenantStoreTerms(ctx, "acme"); got == nil || got.Version != "2027-01" || got.AcceptedAt != "2026-10-25T09:00:00Z" {
		t.Fatalf("after a new version: %+v", got)
	}
	if _, err := g.SetTenantStoreTerms(ctx, "acme", "has space", first, tenantMeta()); !errors.Is(err, gitops.ErrInvalidTermsVersion) {
		t.Fatalf("an invalid version was not refused: %v", err)
	}
}
