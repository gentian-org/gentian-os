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

package applifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// resourcePlanPatchFile is the per-tenant overlay a plan selection writes.
//
// A separate file rather than an edit to tenant.yaml, because tenant.yaml is
// not where a tenant's quotas end up. Every tenant kustomization pulls in the
// shared tenant-defaults component, and a component's patches are applied
// *after* the resources it accompanies — so quotas written into tenant.yaml are
// overwritten by the component's defaults before Argo ever sees them. The edit
// would commit cleanly, push cleanly, sync cleanly, and change nothing, which
// is the failure this repository already has one long comment about in
// app_workload_health.go.
//
// Listing this file under the kustomization's own `patches:` puts it after the
// component in kustomize's order, so it is the last word on the tenant's
// ceiling — which is what a chosen plan has to be.
const resourcePlanPatchFile = "resource-plan.yaml"

// SetResourcePlan writes a tenant's chosen plan into the deployments repository.
func (g *GitOps) SetResourcePlan(
	ctx context.Context,
	tenant string,
	plan *gentianov1alpha1.ResourcePlan,
	actor string,
) (status, file string, changed bool, err error) {
	tenantYAML, err := g.tenantFile(ctx, tenant)
	if err != nil {
		return "", "", false, err
	}
	dir := filepath.Dir(tenantYAML)
	patchPath := filepath.Join(dir, resourcePlanPatchFile)
	kustomizationPath := filepath.Join(dir, "kustomization.yaml")

	desired := renderResourcePlanPatch(tenant, plan)
	existing, readErr := os.ReadFile(patchPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", patchPath, false, readErr
	}

	kustomization, err := os.ReadFile(kustomizationPath)
	if err != nil {
		return "", kustomizationPath, false, fmt.Errorf("read kustomization for %s: %w", tenant, err)
	}
	updatedKustomization, kustomizationChanged := ensurePatchListed(string(kustomization), resourcePlanPatchFile)

	if string(existing) == desired && !kustomizationChanged {
		return "no_change", patchPath, false, nil
	}

	if err := os.WriteFile(patchPath, []byte(desired), 0o644); err != nil {
		return "", patchPath, false, err
	}
	files := []string{patchPath}
	if kustomizationChanged {
		if err := os.WriteFile(kustomizationPath, []byte(updatedKustomization), 0o644); err != nil {
			return "", kustomizationPath, false, err
		}
		files = append(files, kustomizationPath)
	}

	msg := fmt.Sprintf("feat(%s): set resource plan %s (via %s)", tenant, plan.Name, actor)
	if err := g.commitAll(ctx, files, msg); err != nil {
		return "", patchPath, false, err
	}
	return "updated", patchPath, true, nil
}

// renderResourcePlanPatch produces the strategic-merge patch for one plan.
//
// Every quota key a plan can set is emitted, and the ones this plan leaves unset
// are emitted as null — which is how a strategic merge *removes* a key. Omitting
// them instead would leave the tenant-defaults value in place, and the tenant
// would run on a ceiling that is neither the default nor the plan but a silent
// mixture of both: the plan's CPU with the default's storage, priced as the plan.
//
// maxApps is absent from both lists, deliberately. A plan is a quantity of
// capacity — the fields that make one are the fields that become ResourceQuota
// keys, and maxApps becomes none; the Tenant webhook enforces it against
// spec.apps instead. Nulling it here would have a plan change quietly delete a
// cluster's app cap, which is a policy decision no purchase should make.
func renderResourcePlanPatch(tenant string, plan *gentianov1alpha1.ResourcePlan) string {
	q := plan.Spec.Quotas
	var b strings.Builder

	b.WriteString("# Managed by the Gentian resources API — edit through\n")
	b.WriteString("#   kubectl gentian resources set " + tenant + " --plan <plan>\n")
	b.WriteString("# or the Admin Console's Resources tab, so the change is a priced plan\n")
	b.WriteString("# and not an unbilled ceiling. Hand edits here are honoured by the\n")
	b.WriteString("# cluster and reported as drift by the console.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("  annotations:\n")
	b.WriteString("    " + gentianov1alpha1.ResourcePlanAnnotation + ": " + plan.Name + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  quotas:\n")
	b.WriteString(quantityLine("requestsCpu", q.RequestsCPU))
	b.WriteString(quantityLine("requestsMemory", q.RequestsMemory))
	b.WriteString(quantityLine("cpu", q.CPU))
	b.WriteString(quantityLine("memory", q.Memory))
	b.WriteString(quantityLine("storage", q.Storage))
	b.WriteString(countLine("maxPods", q.MaxPods))
	return b.String()
}

func quantityLine(key string, q *resource.Quantity) string {
	if q == nil {
		return "    " + key + ": null\n"
	}
	// Quoted unconditionally: a bare 32 is an integer to YAML and a bare 1e3
	// is a float, and the Tenant CRD's quantity fields accept neither shape
	// back without a conversion that loses the unit.
	return "    " + key + ": \"" + q.String() + "\"\n"
}

func countLine(key string, v int32) string {
	if v <= 0 {
		return "    " + key + ": null\n"
	}
	return fmt.Sprintf("    %s: %d\n", key, v)
}

// ensurePatchListed adds the patch to the kustomization's `patches:` list when
// it is not already there, reporting whether the file changed.
//
// A line editor for the same reason rewriteAddons is one: these files are
// hand-maintained and carry comments explaining why each component is pulled
// in, and a load/dump round trip would reflow all of it into an unreviewable
// diff the first time anyone changes a plan.
func ensurePatchListed(text, patchFile string) (string, bool) {
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
		// Insert at the end of the existing list, not the top: patches apply in
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

// commitAll stages several files and pushes them as one commit.
//
// One commit rather than two, because the patch file and the kustomization that
// lists it are only correct together: a repository synced between the two would
// either apply a patch nothing references or reference a patch that is not
// there, and Argo would fail the whole tenant on the second.
func (g *GitOps) commitAll(ctx context.Context, files []string, message string) error {
	rels := make([]string, 0, len(files))
	for _, f := range files {
		rel, err := filepath.Rel(g.path, f)
		if err != nil {
			rel = f
		}
		rels = append(rels, rel)
	}
	return g.commitPaths(ctx, rels, message)
}
