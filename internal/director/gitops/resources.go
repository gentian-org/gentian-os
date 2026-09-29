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

package gitops

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A tenant's resource plan, as a commit.
//
// The plan is a name and the quantities it sells. The name goes into an
// annotation so the ceiling can say which plan it is even when two plans
// carry the same numbers; the quantities go into spec.quotas, which is what
// the operator turns into the namespace's ResourceQuota. Who chose it goes in
// beside the plan, because the operator reads the Tenant and not git, and it
// is the operator that writes the plan change into the tenant's usage history
// once the change has landed.

// Plan is what a tenant is moved onto.
//
// Handed in by the API from the operator's catalogue rather than read here:
// the catalogue is cluster state, and the director reads git and the
// authorization graph, not the cluster.
type Plan struct {
	Name string
	// Quotas are the plan's quantities by their spec.quotas key: requestsCpu,
	// requestsMemory, cpu, memory, storage and maxPods. A key the plan does
	// not set is absent, and is written as null so the ceiling the tenant
	// ends up with is the plan's and not a mixture of the plan and whatever
	// the manifest had before.
	Quotas map[string]string
}

// ResourcePlanFile is the per-tenant patch a plan selection writes, beside
// tenant.yaml and listed under the kustomization's patches.
//
// A patch rather than an edit to tenant.yaml, so that the plan is one file a
// reviewer can read on its own and the manifest's own comments are left
// alone; and listed under patches so that it is the last word on the quotas
// whatever else the kustomization pulls in.
const ResourcePlanFile = "resource-plan.yaml"

// quotaKeys is every quantity a plan can set, in the order the patch writes
// them. Fixed here because the patch has to name each one, present or null.
var quotaKeys = []string{"requestsCpu", "requestsMemory", "cpu", "memory", "storage", "maxPods"}

// SetResourcePlan writes the tenant's plan as one commit: the patch, and the
// kustomization entry that applies it.
//
// One commit for both files, because they are only correct together: a
// repository synced between the two would apply a patch nothing lists or
// list a patch that is not there, and Argo CD would fail the tenant on the
// second. The retry loop is apply's: a push the remote refuses because it
// moved is a sync and another attempt, not a merge.
func (g *GitOps) SetResourcePlan(ctx context.Context, tenant string, plan Plan, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if strings.TrimSpace(plan.Name) == "" {
		return Result{}, fmt.Errorf("%w: plan name is empty", ErrInvalidName)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for attempt := 1; attempt <= maxPushAttempts; attempt++ {
		manifest, err := g.tenantFile(ctx, tenant)
		if err != nil {
			return Result{}, err
		}
		dir := filepath.Dir(manifest)
		patchPath := filepath.Join(dir, ResourcePlanFile)
		kustomizationPath := filepath.Join(dir, "kustomization.yaml")

		desired := renderResourcePlan(tenant, plan, meta)
		existing, err := os.ReadFile(patchPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		kustomization, err := os.ReadFile(kustomizationPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		listed, kustomizationChanged := ensurePatchListed(string(kustomization), ResourcePlanFile)

		if samePlan(string(existing), desired) && !kustomizationChanged {
			return Result{Status: "unchanged"}, nil
		}
		if err := os.WriteFile(patchPath, []byte(desired), 0o644); err != nil {
			return Result{}, err
		}
		files := []string{patchPath}
		if kustomizationChanged {
			if err := os.WriteFile(kustomizationPath, []byte(listed), 0o644); err != nil {
				return Result{}, err
			}
			files = append(files, kustomizationPath)
		}
		rels := make([]string, 0, len(files))
		for _, f := range files {
			rel, err := filepath.Rel(g.path, f)
			if err != nil {
				return Result{}, err
			}
			rels = append(rels, rel)
		}
		err = g.commitPaths(ctx, rels, fmt.Sprintf("Set resource plan %s for tenant %s", plan.Name, tenant), meta)
		if err == nil {
			return g.landed(ctx, "updated")
		}
		if !errors.Is(err, errPushRejected) {
			return Result{}, err
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(time.Duration(rand.Int63n(int64(attempt) * int64(40*time.Millisecond)))):
		}
	}
	return Result{}, ErrPushContended
}

// samePlan reports whether the patch already says what desired says, apart
// from who set it. Re-choosing the plan a tenant is on is not a change to the
// tenant, and must not be a commit that only renames the chooser.
func samePlan(existing, desired string) bool {
	return withoutSetBy(existing) == withoutSetBy(desired)
}

func withoutSetBy(text string) string {
	lines := strings.Split(text, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.Contains(l, "resource-plan-set-by:") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// renderResourcePlan produces the strategic-merge patch for one plan.
//
// Every quota key is emitted, and the ones this plan leaves unset are emitted
// as null, which is how a strategic merge removes a key. Omitting them would
// leave whatever the manifest had, and the tenant would run on a ceiling that
// is neither the manifest's nor the plan's but a mixture priced as the plan.
//
// maxApps is not among them, deliberately. A plan is a quantity of capacity,
// the fields that become ResourceQuota keys, and maxApps becomes none: the
// Tenant webhook enforces it against spec.apps. A plan change nulling it
// would quietly delete a cluster's app cap, which no purchase should do.
func renderResourcePlan(tenant string, plan Plan, meta Meta) string {
	var b strings.Builder
	b.WriteString("# Managed by the director: this tenant's resource plan, chosen in the\n")
	b.WriteString("# administration console by whoever the commit names. Edit it there, so\n")
	b.WriteString("# the change is a priced plan and not an unbilled ceiling. Hand edits are\n")
	b.WriteString("# honoured by the cluster and reported as drift by the console.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("  annotations:\n")
	b.WriteString("    gentianos.io/resource-plan: " + plan.Name + "\n")
	if actor := meta.actor(); actor != "" {
		b.WriteString("    gentianos.io/resource-plan-set-by: " + quoteScalar(actor) + "\n")
	}
	b.WriteString("spec:\n")
	b.WriteString("  quotas:\n")
	for _, key := range quotaKeys {
		v, ok := plan.Quotas[key]
		switch {
		case !ok || strings.TrimSpace(v) == "":
			b.WriteString("    " + key + ": null\n")
		case key == "maxPods":
			b.WriteString("    " + key + ": " + strings.TrimSpace(v) + "\n")
		default:
			// Quoted unconditionally: a bare 32 is an integer to YAML and a
			// bare 1e3 a float, and the CRD's quantity fields take neither
			// back without a conversion that loses the unit.
			b.WriteString("    " + key + ": \"" + strings.TrimSpace(v) + "\"\n")
		}
	}
	return b.String()
}

// ensurePatchListed adds the patch to the kustomization's patches list when
// it is not already there, reporting whether the text changed. Empty text is
// a tenant directory without a kustomization, and gets the one every tenant
// starts with.
//
// A line editor rather than a load/dump round trip, because these files are
// hand-maintained and carry comments, and a reflow of all of them is not a
// reviewable diff for a one-line change.
func ensurePatchListed(text, patchFile string) (string, bool) {
	if strings.TrimSpace(text) == "" {
		return tenantKustomization() + "patches:\n- path: " + patchFile + "\n", true
	}
	entry := "- path: " + patchFile
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == entry {
			return text, false
		}
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "patches:" {
			continue
		}
		// At the end of the existing list, not the top: patches apply in
		// order and the plan must be the last word on the tenant's quotas.
		end := i + 1
		for end < len(lines) {
			trimmed := strings.TrimSpace(lines[end])
			if trimmed == "" || (!strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(lines[end], " ")) {
				break
			}
			end++
		}
		out := append([]string{}, lines[:end]...)
		out = append(out, entry)
		out = append(out, lines[end:]...)
		return strings.Join(out, "\n") + "\n", true
	}
	return strings.Join(lines, "\n") + "\npatches:\n" + entry + "\n", true
}

// tenantKustomization is the kustomization every tenant directory starts
// with: the manifest, and nothing else until a plan is chosen.
func tenantKustomization() string {
	return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- tenant.yaml\n"
}
