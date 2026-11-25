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
	"os"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// The list is written into the tenant's manifest sorted, as the Tenant's
// schema reads it, and goes from the manifest with its last word.
func TestATenantsTrustedDomainsAreWrittenAsTheTenantReadsThem(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.AddTenantTrustedDomain(ctx, "demo", "example.org", repoMeta()); !errors.Is(err, gitops.ErrTrustedDomainsNotDelegated) {
		t.Fatalf("not delegated: %v", err)
	}
	if res, err := g.SetTenantTrustedDomainsDelegation(ctx, "demo", true, repoMeta()); err != nil || !res.Changed {
		t.Fatalf("delegating: %+v %v", res, err)
	}
	for _, d := range []string{"github.com", "example.org"} {
		if res, err := g.AddTenantTrustedDomain(ctx, "demo", d, repoMeta()); err != nil || !res.Changed {
			t.Fatalf("%s: %+v %v", d, res, err)
		}
	}
	if _, err := g.AddTenantTrustedDomain(ctx, "demo", "Example.ORG", repoMeta()); !errors.Is(err, gitops.ErrTrustedDomainInvalid) {
		t.Fatalf("upper case: %v", err)
	}
	file, err := g.TenantFile(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := "  trustedDomains:\n    delegated: true\n    domains:\n      - \"example.org\"\n      - \"github.com\"\n"
	if !strings.Contains(string(raw), want) && !strings.Contains(string(raw), strings.ReplaceAll(want, `"`, "")) {
		t.Fatalf("the manifest:\n%s", raw)
	}
	have, err := g.TenantTrustedDomains(ctx, "demo")
	if err != nil || !have.Delegated || strings.Join(have.Domains, " ") != "example.org github.com" {
		t.Fatalf("read back %+v %v", have, err)
	}

	for _, d := range []string{"github.com", "example.org"} {
		if _, err := g.RemoveTenantTrustedDomain(ctx, "demo", d, repoMeta()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.RemoveTenantTrustedDomain(ctx, "demo", "example.org", repoMeta()); !errors.Is(err, gitops.ErrTrustedDomainNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
	if _, err := g.SetTenantTrustedDomainsDelegation(ctx, "demo", false, repoMeta()); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(file)
	if strings.Contains(string(raw), "trustedDomains") {
		t.Fatalf("an empty list stayed in the manifest:\n%s", raw)
	}
}
