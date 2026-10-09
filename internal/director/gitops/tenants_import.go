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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// ImportFile is the record of an import that has not finished, beside the
// manifest of the tenant it made. Not a resource: the directory's
// kustomization does not list it and nothing applies it.
const ImportFile = "import.json"

// PendingImport is an import a director began: which bundle, of which
// tenant, and the name of the restore that fills the tenant from it. It is
// committed with the tenant's manifest and removed when the import has
// finished, so that a director that restarts in between knows it has one to
// finish -- as a purge is known by its annotation (PendingPurges).
//
// It holds no key. The key a bundle is opened with is given with the request
// and is not written to git.
type PendingImport struct {
	Tenant string `json:"tenant"`
	// Source and Export are the tenant the bundle was taken of and the
	// export that wrote it, as its manifest records them.
	Source string `json:"source,omitempty"`
	Export string `json:"export,omitempty"`
	// Bundle is where the bundle is.
	Bundle gentianov1alpha1.BundleRef `json:"bundle"`
	// Restore is the name of the restore the import starts. Decided when the
	// import is, so that a director that restarts can ask whether it was
	// started and never starts a second.
	Restore string `json:"restore"`
	// RequestedAt and RequestedBy say when and by whom.
	RequestedAt string `json:"requestedAt,omitempty"`
	RequestedBy string `json:"requestedBy,omitempty"`
}

// ImportedTenant is a tenant to be made from a bundle.
type ImportedTenant struct {
	// Name is the tenant's name here: the bundle's, or another.
	Name string
	// Spec is the tenant's settings as the bundle's manifest carries them.
	Spec *gentianov1alpha1.TenantSpec
	// Origin says which bundle, for the manifest's header.
	Origin string
	// Pending is recorded beside the manifest.
	Pending PendingImport
}

// DeclareTenant is CreateTenant from a bundle's manifest: the settings the
// bundle carried, written as the tenant's manifest and committed by the one
// function a tenant is created by (createTenant), so the operator provisions
// the shells a restore then fills.
//
// What is not the bundle's to decide:
//
//   - The tenant's names. Its realm, its database prefix and its bucket
//     prefix follow from its name here by the platform's one rule
//     (backup.RuleIsolation), as a created tenant's do. A bundle's manifest
//     states the names of the tenant it was taken of; copied, a tenant
//     imported under another name beside the original pointed at the
//     original's realm, databases and buckets, and the restore that followed
//     replaced them. The placement of its workloads (isolation.namespace) is
//     not carried either.
//   - deletionPolicy is Retain: a tenant that was exported while a purge was
//     under way must not arrive with its purge; and spec.deletion is dropped
//     with it.
//   - spec.catalogue is dropped: a bundle does not bring its own catalogues
//     or its own delegation.
//   - spec.perimeter is dropped: whether a tenant's own administrators
//     approve its public addresses is not the bundle's to say either.
func (g *GitOps) DeclareTenant(ctx context.Context, imported ImportedTenant, meta Meta) (Result, error) {
	name := imported.Name
	if imported.Spec == nil {
		return Result{}, fmt.Errorf("the bundle carries no tenant spec")
	}
	pending := imported.Pending
	pending.Tenant = name
	record, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return Result{}, err
	}
	manifest := func() ([]byte, error) {
		declared := imported.Spec.DeepCopy()
		mode := gentianov1alpha1.IsolationMode("")
		if declared.Isolation != nil {
			mode = declared.Isolation.Mode
		}
		declared.Isolation = backup.RuleIsolation(name, mode)
		declared.DeletionPolicy = gentianov1alpha1.DeletionPolicyRetain
		declared.Deletion = nil
		// Nor are its catalogues. Which addresses a tenant installs from, and
		// whether its own administrators may add more, were decided by the
		// administrator of the cluster the bundle came from; on this one they
		// are decided here, through the catalogue routes, after the import.
		declared.Catalogue = nil
		// Nor who approves its public addresses. That the tenant's own
		// administrators did where the bundle came from was that cluster's
		// administrator's decision; here it is off until this one's turns
		// it on (SetTenantAdminsApprove).
		declared.Perimeter = nil
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
			return nil, err
		}
		header := fmt.Sprintf(`# Tenant %s, imported from a bundle through the director.
#
# The settings are the ones the bundle carried (%s); the names under
# isolation are this tenant's own, by its name. Argo CD syncs this file and
# the operator provisions the tenant as empty shells; the restore the import
# started then fills them from the bundle. Editing this file is how the
# tenant changes from here on.
`, name, imported.Origin)
		return append([]byte(header), body...), nil
	}
	return g.createTenant(ctx, name, manifest, map[string][]byte{ImportFile: append(record, '\n')},
		fmt.Sprintf("Import tenant %s", name), "imported", meta)
}

// PendingImports are the imports that were begun and have not finished.
func (g *GitOps) PendingImports(ctx context.Context) ([]PendingImport, error) {
	names, err := g.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	var out []PendingImport
	for _, name := range names {
		pending, ok, err := g.PendingImport(ctx, name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, pending)
		}
	}
	return out, nil
}

// PendingImport is the unfinished import of one tenant, if it has one.
func (g *GitOps) PendingImport(ctx context.Context, tenant string) (PendingImport, bool, error) {
	file, err := g.TenantFile(ctx, tenant)
	if errors.Is(err, ErrTenantNotFound) {
		return PendingImport{}, false, nil
	}
	if err != nil {
		return PendingImport{}, false, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), ImportFile))
	if errors.Is(err, os.ErrNotExist) {
		return PendingImport{}, false, nil
	}
	if err != nil {
		return PendingImport{}, false, err
	}
	var pending PendingImport
	if err := json.Unmarshal(raw, &pending); err != nil {
		return PendingImport{}, false, fmt.Errorf("the record of the import of tenant %s does not parse: %w", tenant, err)
	}
	pending.Tenant = tenant
	return pending, true, nil
}

// FinishImport removes the record of a tenant's import: it has finished.
func (g *GitOps) FinishImport(ctx context.Context, tenant string, meta Meta) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFile(ctx, tenant)
	if errors.Is(err, ErrTenantNotFound) {
		return Result{Status: "no_import"}, nil
	}
	if err != nil {
		return Result{}, err
	}
	record := filepath.Join(filepath.Dir(file), ImportFile)
	if _, err := os.Stat(record); errors.Is(err, os.ErrNotExist) {
		return Result{Status: "no_import"}, nil
	}
	if err := os.Remove(record); err != nil {
		return Result{}, err
	}
	rel, err := filepath.Rel(g.path, record)
	if err != nil {
		return Result{}, err
	}
	if err := g.commitPaths(ctx, []string{rel}, fmt.Sprintf("Import of tenant %s finished", tenant), meta); err != nil {
		return Result{}, err
	}
	return g.landed(ctx, "import_finished")
}
