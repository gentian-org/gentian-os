/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The cluster's resource plans, as files.
//
// One ResourcePlan per file under clusters/<cluster>/resource-plans/, listed
// in that directory's kustomization, which the gentian-resource-plans
// Application syncs with pruning (kernel/appsets/raw/11c-resource-plans.yaml).
// The director writes them for whoever may configure the cluster; the
// operator reads the objects Argo CD made of them. The rules a change has to
// keep (a plan's quantities do not change under the tenants on it, one
// default) need the operator's view of who is on what, and are the API's.

// PlanCatalogDir is the directory of the cluster's plans.
const PlanCatalogDir = "resource-plans"

// ErrPlanNotFound is a plan git does not hold.
var ErrPlanNotFound = errors.New("no such resource plan on this cluster")

// ErrInvalidPlan is a plan that cannot be written as it is.
var ErrInvalidPlan = errors.New("invalid resource plan")

// PlanDefinition is one plan as the API reads and writes it.
type PlanDefinition struct {
	Name                string            `json:"name"`
	DisplayName         string            `json:"displayName"`
	Description         string            `json:"description,omitempty"`
	Tier                int32             `json:"tier"`
	Units               int32             `json:"units,omitempty"`
	ProductSku          string            `json:"productSku,omitempty"`
	Default             bool              `json:"default,omitempty"`
	SelfServiceDisabled bool              `json:"selfServiceDisabled,omitempty"`
	Retired             bool              `json:"retired,omitempty"`
	Quotas              map[string]string `json:"quotas"`
}

// CheckPlanDefinition refuses a plan the CRD would refuse, or the operator
// could not enforce, before anything is written.
func CheckPlanDefinition(p *PlanDefinition) error {
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.Description = strings.TrimSpace(p.Description)
	p.ProductSku = strings.TrimSpace(p.ProductSku)
	switch {
	case !ValidName(p.Name):
		return fmt.Errorf("%w: the name is lower-case letters, digits and hyphens", ErrInvalidPlan)
	case p.DisplayName == "" || len(p.DisplayName) > 128 || strings.ContainsAny(p.DisplayName, "\r\n"):
		return fmt.Errorf("%w: displayName is one line of 1 to 128 characters", ErrInvalidPlan)
	case len(p.Description) > 512:
		return fmt.Errorf("%w: description is 512 characters at most", ErrInvalidPlan)
	case p.Tier < 0:
		return fmt.Errorf("%w: tier is 0 or more", ErrInvalidPlan)
	case p.Units < 0:
		return fmt.Errorf("%w: units is 0 or more", ErrInvalidPlan)
	case len(p.ProductSku) > 128 || strings.ContainsAny(p.ProductSku, " \t\r\n"):
		return fmt.Errorf("%w: productSku is one word of 128 characters at most", ErrInvalidPlan)
	case p.Default && p.Retired:
		return fmt.Errorf("%w: the default plan cannot be retired", ErrInvalidPlan)
	}
	if len(p.Quotas) == 0 {
		return fmt.Errorf("%w: a plan sets at least one quota", ErrInvalidPlan)
	}
	known := map[string]bool{}
	for _, k := range quotaKeys {
		known[k] = true
	}
	clean := map[string]string{}
	for k, v := range p.Quotas {
		v = strings.TrimSpace(v)
		if !known[k] {
			return fmt.Errorf("%w: unknown quota %q (one of %s)", ErrInvalidPlan, k, strings.Join(quotaKeys, ", "))
		}
		if v == "" {
			continue
		}
		if k == "maxPods" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return fmt.Errorf("%w: maxPods is a whole number above 0", ErrInvalidPlan)
			}
			clean[k] = strconv.Itoa(n)
			continue
		}
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("%w: %s is a quantity above 0, such as 4 or 16Gi", ErrInvalidPlan, k)
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		return fmt.Errorf("%w: a plan sets at least one quota", ErrInvalidPlan)
	}
	p.Quotas = clean
	return nil
}

// SameQuotas reports whether two plans set the same quantities, compared as
// quantities: 1Gi and 1024Mi are one ceiling.
func SameQuotas(a, b map[string]string) bool {
	for _, k := range quotaKeys {
		va, oka := a[k]
		vb, okb := b[k]
		if oka != okb {
			return false
		}
		if !oka || va == vb {
			continue
		}
		qa, errA := resource.ParseQuantity(va)
		qb, errB := resource.ParseQuantity(vb)
		if errA != nil || errB != nil || qa.Cmp(qb) != 0 {
			return false
		}
	}
	return true
}

func planFile(name string) string { return name + ".yaml" }

func (g *GitOps) planDir() string {
	return filepath.Join(g.path, "clusters", g.cluster, PlanCatalogDir)
}

// PlanDefinitions reads every plan git holds, smallest tier first.
func (g *GitOps) PlanDefinitions(ctx context.Context) ([]PlanDefinition, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepoRead(ctx); err != nil {
		return nil, err
	}
	return g.planDefinitionsLocked()
}

func (g *GitOps) planDefinitionsLocked() ([]PlanDefinition, error) {
	entries, err := os.ReadDir(g.planDir())
	if errors.Is(err, os.ErrNotExist) {
		return []PlanDefinition{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []PlanDefinition{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || e.Name() == "kustomization.yaml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(g.planDir(), e.Name()))
		if err != nil {
			return nil, err
		}
		var doc gentianov1alpha1.ResourcePlan
		if err := yaml.Unmarshal(raw, &doc); err != nil || doc.Kind != "ResourcePlan" || doc.Name == "" {
			continue
		}
		out = append(out, PlanDefinition{
			Name:                doc.Name,
			DisplayName:         doc.Spec.DisplayName,
			Description:         doc.Spec.Description,
			Tier:                doc.Spec.Tier,
			Units:               doc.Spec.Units,
			ProductSku:          doc.Spec.ProductSku,
			Default:             doc.Spec.Default,
			SelfServiceDisabled: doc.Spec.SelfServiceDisabled,
			Retired:             doc.Spec.Retired,
			Quotas:              quotasOf(&doc.Spec.Quotas),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier < out[j].Tier
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func quotasOf(q *gentianov1alpha1.TenantQuotas) map[string]string {
	out := map[string]string{}
	set := func(k string, v *resource.Quantity) {
		if v != nil {
			out[k] = v.String()
		}
	}
	set("requestsCpu", q.RequestsCPU)
	set("requestsMemory", q.RequestsMemory)
	set("cpu", q.CPU)
	set("memory", q.Memory)
	set("storage", q.Storage)
	if q.MaxPods > 0 {
		out["maxPods"] = strconv.Itoa(int(q.MaxPods))
	}
	return out
}

// PlanWrite is one plan to write, or to remove when Remove is set.
type PlanWrite struct {
	Plan   PlanDefinition
	Remove bool
}

// WritePlans writes or removes plans and keeps the directory's kustomization
// in step, as one commit: moving the default from one plan to another is two
// files that are only right together.
func (g *GitOps) WritePlans(ctx context.Context, message string, meta Meta, writes ...PlanWrite) (Result, error) {
	for i := range writes {
		if writes[i].Remove {
			if !ValidName(writes[i].Plan.Name) {
				return Result{}, fmt.Errorf("%w: %q", ErrInvalidName, writes[i].Plan.Name)
			}
			continue
		}
		if err := CheckPlanDefinition(&writes[i].Plan); err != nil {
			return Result{}, err
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	return g.writePlansLocked(ctx, message, meta, writes)
}

func (g *GitOps) writePlansLocked(ctx context.Context, message string, meta Meta, writes []PlanWrite) (Result, error) {
	dir := g.planDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	kustomizationPath := filepath.Join(dir, "kustomization.yaml")
	kustomization, err := os.ReadFile(kustomizationPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	listed := string(kustomization)
	if strings.TrimSpace(listed) == "" {
		listed = planKustomization()
	}
	changed := listed != string(kustomization)
	paths := []string{}
	for _, w := range writes {
		// The name becomes a file name: checked here, where the path is
		// made, and not only where the plan was, so no path a caller
		// supplied reaches the checkout.
		if !dnsLabel.MatchString(w.Plan.Name) {
			return Result{}, fmt.Errorf("%w: %q", ErrInvalidName, w.Plan.Name)
		}
		path := filepath.Join(dir, planFile(w.Plan.Name))
		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		var kchanged bool
		if w.Remove {
			if errors.Is(err, os.ErrNotExist) {
				return Result{}, ErrPlanNotFound
			}
			if err := os.Remove(path); err != nil {
				return Result{}, err
			}
			listed, kchanged = removeListed(listed, planFile(w.Plan.Name), listResource)
			paths = append(paths, path)
		} else {
			body := renderPlan(w.Plan)
			listed, kchanged = ensureListed(listed, planFile(w.Plan.Name), listResource)
			if string(existing) != body {
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					return Result{}, err
				}
				paths = append(paths, path)
			}
		}
		changed = changed || kchanged
	}
	if changed {
		listed = emptyResourcesAsList(listed)
		if err := os.WriteFile(kustomizationPath, []byte(listed), 0o644); err != nil {
			return Result{}, err
		}
		paths = append(paths, kustomizationPath)
	}
	if len(paths) == 0 {
		return Result{Status: "unchanged"}, nil
	}
	rels := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(g.path, p)
		if err != nil {
			return Result{}, err
		}
		rels = append(rels, rel)
	}
	if err := g.commitPaths(ctx, rels, message, meta); err != nil {
		return Result{}, err
	}
	return g.landed(ctx, "updated")
}

// SeedPlans starts the cluster's plans from the deployment's values (the
// operator chart's usage.plans.catalogue), once: only while git holds no
// plan at all. The chart no longer creates the objects on a cluster with a
// director, so from here on these files are the only catalog, edited
// through the API. The director refuses to remove the default plan, so a
// catalog somebody edited is never empty and never started again.
func (g *GitOps) SeedPlans(ctx context.Context, plans []PlanDefinition) (Result, error) {
	writes := make([]PlanWrite, 0, len(plans))
	for _, p := range plans {
		if err := CheckPlanDefinition(&p); err != nil {
			return Result{}, fmt.Errorf("plan %s: %w", p.Name, err)
		}
		writes = append(writes, PlanWrite{Plan: p})
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	existing, err := g.planDefinitionsLocked()
	if err != nil {
		return Result{}, err
	}
	if len(existing) > 0 {
		return Result{Status: "unchanged"}, nil
	}
	return g.writePlansLocked(ctx, "Start this cluster's resource plans from the deployment's values",
		Meta{Principal: "gentian-director", Decision: "usage.plans.catalogue of the deployment's values"}, writes)
}

// emptyResourcesAsList writes a resources key left without entries as the
// empty list step 0 scaffolds, rather than as a key with no value.
func emptyResourcesAsList(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "resources:" {
			continue
		}
		if i+1 >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "-") {
			lines[i] = "resources: []"
		}
	}
	return strings.Join(lines, "\n")
}

func planKustomization() string {
	return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"
}

// renderPlan writes the object a reviewer reads in the commit.
func renderPlan(p PlanDefinition) string {
	var b strings.Builder
	b.WriteString("# Managed by the director: one of this cluster's resource plans, set by\n")
	b.WriteString("# whoever the commit names. Its quotas do not change while a tenant is on\n")
	b.WriteString("# it; a different ceiling is a new plan.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: ResourcePlan\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + p.Name + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  displayName: " + strconv.Quote(p.DisplayName) + "\n")
	if p.Description != "" {
		b.WriteString("  description: " + strconv.Quote(p.Description) + "\n")
	}
	b.WriteString("  tier: " + strconv.Itoa(int(p.Tier)) + "\n")
	if p.Units > 0 {
		b.WriteString("  units: " + strconv.Itoa(int(p.Units)) + "\n")
	}
	if p.ProductSku != "" {
		b.WriteString("  productSku: " + strconv.Quote(p.ProductSku) + "\n")
	}
	if p.Default {
		b.WriteString("  default: true\n")
	}
	if p.SelfServiceDisabled {
		b.WriteString("  selfServiceDisabled: true\n")
	}
	if p.Retired {
		b.WriteString("  retired: true\n")
	}
	b.WriteString("  quotas:\n")
	for _, k := range quotaKeys {
		v, ok := p.Quotas[k]
		if !ok {
			continue
		}
		if k == "maxPods" {
			b.WriteString("    " + k + ": " + v + "\n")
			continue
		}
		b.WriteString("    " + k + ": \"" + v + "\"\n")
	}
	return b.String()
}
