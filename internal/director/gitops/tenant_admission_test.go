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

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// A change that leaves a tenant's manifest as the cluster would refuse it is
// not committed, and the checkout is left where the remote is; the cluster is
// shown the manifest as the change leaves it. A change the cluster takes is
// committed, and so is one made while the cluster cannot be asked.
func TestATenantTheClusterWouldRefuseIsNotCommitted(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	tip := func() string { return dt.Git(t, "", "--git-dir", remote, "rev-parse", "main") }

	var shown, unasked []string
	answer, unreachable := "", error(nil)
	g.GuardTenants(func(_ context.Context, tenant string, manifest []byte) (string, error) {
		shown = append(shown, tenant+"\n"+string(manifest))
		return answer, unreachable
	}, func(tenant string, _ error) { unasked = append(unasked, tenant) })

	before := tip()
	answer = `admission webhook "vtenant.gentianos.io" denied the request: no`
	_, err := g.InstallFrom(ctx, "demo", "element", "", "", meta("u-ada"))
	var refused *gitops.TenantRefused
	if !errors.As(err, &refused) || refused.Tenant != "demo" || !strings.Contains(refused.Reason, "denied the request") {
		t.Fatalf("install = %v", err)
	}
	if tip() != before {
		t.Fatal("a refused change was committed")
	}
	if len(shown) != 1 || !strings.HasPrefix(shown[0], "demo\n") || !strings.Contains(shown[0], "- profile: element") {
		t.Fatalf("the cluster was shown %q", shown)
	}

	answer = ""
	if _, err := g.InstallFrom(ctx, "demo", "element", "", "", meta("u-ada")); err != nil || tip() == before {
		t.Fatalf("a change the cluster takes: %v, committed %v", err, tip() != before)
	}

	before, unreachable = tip(), errors.New("the operator is not there")
	if _, err := g.InstallFrom(ctx, "demo", "wiki", "", "", meta("u-ada")); err != nil || tip() == before {
		t.Fatalf("a change made while the cluster cannot be asked: %v, committed %v", err, tip() != before)
	}
	if len(unasked) != 1 || unasked[0] != "demo" {
		t.Fatalf("told of the unasked: %v", unasked)
	}
}
