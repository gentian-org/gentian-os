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
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// DeclareTenant is CreateTenant from a bundle's manifest: the TenantSpec the
// bundle carried, written as the tenant's manifest and committed. The same
// commit a form would have produced, with the apps list the bundle recorded,
// so the operator provisions the shells a restore then fills (§4.3).
//
// Two fields are not the bundle's to decide. deletionPolicy is Retain: a
// tenant that was exported while a purge was under way must not arrive with
// its purge; and spec.deletion is dropped with it.
func (g *GitOps) DeclareTenant(ctx context.Context, name string, spec *gentianov1alpha1.TenantSpec, origin string, meta Meta) (Result, error) {
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, name)
	}
	if spec == nil {
		return Result{}, fmt.Errorf("the bundle carries no tenant spec")
	}
	if err := g.refuseInSingleTenancy(ctx); err != nil {
		return Result{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	dir := filepath.Join(g.path, "clusters", cluster, "tenants", name)
	file := filepath.Join(dir, "tenant.yaml")
	if _, err := os.Stat(file); err == nil {
		return Result{}, fmt.Errorf("%w: %q", ErrTenantExists, name)
	}
	declared := spec.DeepCopy()
	declared.DeletionPolicy = gentianov1alpha1.DeletionPolicyRetain
	declared.Deletion = nil
	body, err := yaml.Marshal(map[string]any{
		"apiVersion": "gentianos.io/v1alpha1",
		"kind":       "Tenant",
		"metadata": map[string]any{
			"name":        name,
			"annotations": map[string]string{"argocd.argoproj.io/sync-wave": "2"},
		},
		"spec": declared,
	})
	if err != nil {
		return Result{}, err
	}
	header := fmt.Sprintf(`# Tenant %s, imported from a bundle through the director.
#
# The spec is the one the bundle carried (%s). Argo CD syncs this file and
# the operator provisions the tenant as empty shells; the restore the import
# started then fills them from the bundle. Editing this file is how the
# tenant changes from here on.
`, name, origin)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(file, append([]byte(header), body...), 0o644); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(tenantKustomization()), 0o644); err != nil {
		return Result{}, err
	}
	rels := []string{}
	for _, f := range []string{file, filepath.Join(dir, "kustomization.yaml")} {
		rel, err := filepath.Rel(g.path, f)
		if err != nil {
			return Result{}, err
		}
		rels = append(rels, rel)
	}
	if err := g.commitPaths(ctx, rels, fmt.Sprintf("Import tenant %s", name), meta); err != nil {
		return Result{}, err
	}
	return g.landed(ctx, "imported")
}
