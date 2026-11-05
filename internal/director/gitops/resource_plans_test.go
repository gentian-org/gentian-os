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
	"os"
	"path/filepath"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

func seedPlans() []gitops.PlanDefinition {
	return []gitops.PlanDefinition{
		{Name: "base", DisplayName: "Base", Tier: 0, Units: 1, Default: true, ProductSku: "sku-1",
			Quotas: map[string]string{"requestsCpu": "4", "requestsMemory": "16Gi", "cpu": "16", "memory": "32Gi", "storage": "50Gi"}},
		{Name: "nodes-2", DisplayName: "2 nodes", Tier: 10, Units: 2,
			Quotas: map[string]string{"requestsCpu": "8", "maxPods": "60"}},
	}
}

// The deployment's values start the catalog once, into the empty directory
// step 0 scaffolds, and never again: from then on the files are the catalog.
func TestThePlansAreStartedFromTheValuesOnce(t *testing.T) {
	ctx := context.Background()
	remote := dt.Remote(t, "demo")
	checkout := dt.Clone(t, remote)
	// What step 0 leaves: the directory and an empty kustomization.
	dir := filepath.Join(checkout, "clusters", dt.Cluster, gitops.PlanCatalogDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	empty := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(empty), 0o644); err != nil {
		t.Fatal(err)
	}
	dt.Git(t, checkout, "add", ".")
	dt.Git(t, checkout, "commit", "-m", "step 0")
	dt.Git(t, checkout, "push")
	g := gitops.NewGitOps(checkout, remote, dt.Cluster, director)

	res, err := g.SeedPlans(ctx, seedPlans())
	if err != nil || !res.Changed {
		t.Fatalf("seed: %+v (%v)", res, err)
	}
	plans, err := g.PlanDefinitions(ctx)
	if err != nil || len(plans) != 2 || plans[0].Name != "base" || plans[1].Units != 2 || plans[1].Quotas["maxPods"] != "60" {
		t.Fatalf("read back = %+v (%v)", plans, err)
	}
	kustomization := dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/resource-plans/kustomization.yaml")
	if strings.Contains(kustomization, "[]") || !strings.Contains(kustomization, "- base.yaml") {
		t.Fatalf("kustomization:\n%s", kustomization)
	}

	// Somebody removes a plan; the values do not bring it back.
	if _, err := g.WritePlans(ctx, "Remove resource plan nodes-2", meta("u-ada"), gitops.PlanWrite{Plan: plans[1], Remove: true}); err != nil {
		t.Fatal(err)
	}
	if res, err := g.SeedPlans(ctx, seedPlans()); err != nil || res.Changed {
		t.Fatalf("the values were written over the catalog: %+v (%v)", res, err)
	}
	if plans, _ := g.PlanDefinitions(ctx); len(plans) != 1 {
		t.Fatalf("plans = %+v", plans)
	}
}

// A cluster updated without an install run has no directory yet; the seed
// makes it.
func TestThePlansAreStartedWhereNoDirectoryIs(t *testing.T) {
	ctx := context.Background()
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if res, err := g.SeedPlans(ctx, seedPlans()); err != nil || !res.Changed {
		t.Fatalf("seed: %+v (%v)", res, err)
	}
	if !strings.Contains(dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/resource-plans/nodes-2.yaml"), "units: 2") {
		t.Fatal("nodes-2 not written")
	}
	bad := seedPlans()
	bad[0].Quotas = map[string]string{"cpu": "lots"}
	other := dt.Remote(t, "demo")
	fresh := gitops.NewGitOps(dt.Clone(t, other), other, dt.Cluster, director)
	if _, err := fresh.SeedPlans(ctx, bad); err == nil {
		t.Fatal("a plan with a quota that is no quantity was seeded")
	}
}

// Removing the last plan leaves the empty list step 0 writes, which
// kustomize reads, rather than a resources key with no value.
func TestRemovingTheLastPlanLeavesAnEmptyList(t *testing.T) {
	ctx := context.Background()
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	one := seedPlans()[:1]
	if _, err := g.SeedPlans(ctx, one); err != nil {
		t.Fatal(err)
	}
	if _, err := g.WritePlans(ctx, "Remove", meta("u-ada"), gitops.PlanWrite{Plan: one[0], Remove: true}); err != nil {
		t.Fatal(err)
	}
	if k := dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/resource-plans/kustomization.yaml"); !strings.Contains(k, "resources: []") {
		t.Fatalf("kustomization:\n%s", k)
	}
}
