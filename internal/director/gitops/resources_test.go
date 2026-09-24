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
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

func planMeta(email string) gitops.Meta {
	return gitops.Meta{
		Author:    gitops.Person{Name: "Tom", Email: email},
		Subject:   "u-tom",
		RequestID: "req-plan-1",
		Decision:  "can_set_plan tenant:demo",
	}
}

var nodes2 = gitops.Plan{Name: "nodes-2", Quotas: map[string]string{
	"requestsCpu": "8", "requestsMemory": "32Gi", "cpu": "16", "memory": "64Gi", "storage": "100Gi", "maxPods": "60",
}}

// A plan is two files in one commit: the patch, and the kustomization that
// applies it last. A tenant directory without a kustomization gets one.
func TestChoosingAPlanIsOneCommitOfPatchAndKustomization(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	res, err := g.SetResourcePlan(ctx, "demo", nodes2, planMeta("tom@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "updated" || !res.Changed || res.Commit != dt.Git(t, "", "--git-dir", remote, "rev-parse", "main") {
		t.Fatalf("result = %+v", res)
	}
	dir := "clusters/" + dt.Cluster + "/tenants/demo/"
	patch := dt.RemoteFile(t, remote, dir+"resource-plan.yaml")
	for _, want := range []string{
		"kind: Tenant", "name: demo",
		"gentianos.io/resource-plan: nodes-2",
		`gentianos.io/resource-plan-set-by: "tom@example.com"`,
		`requestsCpu: "8"`, `storage: "100Gi"`, "maxPods: 60",
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch is missing %q:\n%s", want, patch)
		}
	}
	kustomization := dt.RemoteFile(t, remote, dir+"kustomization.yaml")
	if want := "resources:\n- tenant.yaml\npatches:\n- path: resource-plan.yaml"; !strings.HasSuffix(kustomization, want) {
		t.Fatalf("kustomization:\n%s", kustomization)
	}
	// Both files in the one commit.
	files := dt.Git(t, "", "--git-dir", remote, "show", "--name-only", "--format=", "main")
	if !strings.Contains(files, "resource-plan.yaml") || !strings.Contains(files, "kustomization.yaml") {
		t.Fatalf("commit touched:\n%s", files)
	}
}

// The plan git records is the tenant's plan. Choosing it again, by anyone,
// changes nothing; choosing another rewrites the patch and only the patch.
func TestReChoosingTheRecordedPlanIsNotACommit(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	if _, err := g.SetResourcePlan(ctx, "demo", nodes2, planMeta("tom@example.com")); err != nil {
		t.Fatal(err)
	}
	tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	res, err := g.SetResourcePlan(ctx, "demo", nodes2, planMeta("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Status != "unchanged" || dt.Git(t, "", "--git-dir", remote, "rev-parse", "main") != tip {
		t.Fatalf("a re-choice moved the repository: %+v", res)
	}

	nodes1 := gitops.Plan{Name: "nodes-1", Quotas: map[string]string{"requestsCpu": "4", "cpu": "8"}}
	res, err = g.SetResourcePlan(ctx, "demo", nodes1, planMeta("alice@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatalf("result = %+v", res)
	}
	files := dt.Git(t, "", "--git-dir", remote, "show", "--name-only", "--format=", "main")
	if strings.Contains(files, "kustomization.yaml") {
		t.Fatalf("the kustomization was rewritten although it already listed the patch:\n%s", files)
	}
	patch := dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/tenants/demo/resource-plan.yaml")
	// What the plan leaves unset is null, so the ceiling is the plan's alone.
	for _, want := range []string{"resource-plan: nodes-1", `requestsCpu: "4"`, "requestsMemory: null", "memory: null", "storage: null", "maxPods: null"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch is missing %q:\n%s", want, patch)
		}
	}
	if strings.Contains(patch, "maxApps") {
		t.Error("a plan must not touch the app cap")
	}
}

func TestAPlanForAnUnknownTenantOrWithoutANameIsRefused(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	if _, err := g.SetResourcePlan(ctx, "nobody", nodes2, planMeta("tom@example.com")); err == nil {
		t.Fatal("a plan was written for a tenant that does not exist")
	}
	if _, err := g.SetResourcePlan(ctx, "demo", gitops.Plan{Name: " "}, planMeta("tom@example.com")); err == nil {
		t.Fatal("a plan without a name was written")
	}
	if _, err := g.SetResourcePlan(ctx, "Demo!", nodes2, planMeta("tom@example.com")); err == nil {
		t.Fatal("an invalid tenant name was accepted")
	}
}

// A tenant brought on through the director starts as a kustomization, the
// same shape the bootstrap scaffolds, so the day a plan is chosen adds a
// patch to a list rather than turning a plain directory into one.
func TestANewTenantStartsWithItsKustomization(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if _, err := g.CreateTenant(context.Background(), gitops.NewTenant{Name: "acme"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	k := dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/tenants/acme/kustomization.yaml")
	if !strings.Contains(k, "kind: Kustomization") || !strings.Contains(k, "- tenant.yaml") {
		t.Fatalf("kustomization:\n%s", k)
	}
}
