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
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// A backup policy, as a commit.
//
// A policy is declared state: where bundles go, how often, how long they are
// kept and whose key they are readable with. So it is written the way every
// other piece of declared state is — a file in the deployments repository,
// committed by the person who asked, synced by Argo CD, turned into a
// BackupPolicy the reconciler resolves. Taking a backup is not this: that is
// an action, it happens once, and it is not written here (see the director's
// action routes).
//
// The file is a whole object rather than a patch, because unlike a resource
// plan there is nothing underneath it to merge with: a tenant either states a
// policy or inherits one, and "states nothing" is expressed by the file not
// being there at all.

// BackupPolicyFile is the per-scope file a policy is written to.
const BackupPolicyFile = "backup-policy.yaml"

// BackupPolicy is what a caller states. It is deliberately the shape of the
// CRD's spec and not a shape of its own: the director does not interpret a
// policy, it commits one, and the reconciler is what resolves inheritance.
type BackupPolicy struct {
	Destination     *BackupDestination `json:"destination,omitempty"`
	Schedule        string             `json:"schedule,omitempty"`
	SuspendSchedule bool               `json:"suspendSchedule,omitempty"`
	Retention       *BackupRetention   `json:"retention,omitempty"`
	Encryption      *BackupEncryption  `json:"encryption,omitempty"`
	// AllowTenantOverride is read from the cluster's policy only. A tenant
	// cannot grant itself the right to state one.
	AllowTenantOverride *bool `json:"allowTenantOverride,omitempty"`
}

// BackupDestination is where bundles are written.
type BackupDestination struct {
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Region   string `json:"region,omitempty"`
}

// BackupRetention is which bundles survive.
type BackupRetention struct {
	KeepLast    int32 `json:"keepLast,omitempty"`
	KeepDaily   int32 `json:"keepDaily,omitempty"`
	KeepWeekly  int32 `json:"keepWeekly,omitempty"`
	KeepMonthly int32 `json:"keepMonthly,omitempty"`
	KeepYearly  int32 `json:"keepYearly,omitempty"`
}

// BackupEncryption names the age recipients bundles are encrypted to. Empty
// means the platform's own key, which is what makes a restore something the
// platform can help with.
type BackupEncryption struct {
	Recipients []string `json:"recipients,omitempty"`
}

// ErrBackupOverrideRefused is a tenant policy written where the cluster does
// not allow one.
var ErrBackupOverrideRefused = errors.New("this cluster does not allow tenants to state their own backup policy")

// SetTenantBackupPolicy writes one tenant's policy and commits it.
func (g *GitOps) SetTenantBackupPolicy(ctx context.Context, tenant string, policy BackupPolicy, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	// AllowTenantOverride is the cluster's to state. A tenant that sent one
	// has it dropped rather than refused: it changes nothing either way, and
	// refusing a field the screen never shows would be a puzzle.
	policy.AllowTenantOverride = nil
	body := renderBackupPolicy("tenant", tenant, policy)
	return g.writeTenantFile(ctx, tenant, BackupPolicyFile, body,
		fmt.Sprintf("Set the backup policy for tenant %s", tenant), meta)
}

// ClearTenantBackupPolicy removes a tenant's policy, which is how a tenant
// goes back to inheriting the cluster's. Removing the file is the statement:
// there is no "inherit" value to write, because inheriting is what a tenant
// does when it says nothing.
func (g *GitOps) ClearTenantBackupPolicy(ctx context.Context, tenant string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	return g.writeTenantFile(ctx, tenant, BackupPolicyFile, "",
		fmt.Sprintf("Tenant %s inherits the cluster's backup policy", tenant), meta)
}

// SetClusterBackupPolicy writes the cluster's own policy and commits it.
//
// Beside the claims, because that is the directory Argo CD syncs for what
// this cluster declares about itself, and a backup policy is exactly that.
func (g *GitOps) SetClusterBackupPolicy(ctx context.Context, policy BackupPolicy, meta Meta) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	path := filepath.Join(g.path, "clusters", cluster, "kernel", "claims", BackupPolicyFile)
	desired := renderBackupPolicy("cluster", "", policy)
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	if string(existing) == desired {
		return Result{Status: "unchanged"}, nil
	}
	if err := os.WriteFile(path, []byte(desired), 0o644); err != nil {
		return Result{}, err
	}
	rel, err := filepath.Rel(g.path, path)
	if err != nil {
		return Result{}, err
	}
	if err := g.commitPaths(ctx, []string{rel}, "Set the cluster's backup policy", meta); err != nil {
		return Result{}, err
	}
	return g.landed(ctx, "updated")
}

// ClusterBackupPolicy reads what the cluster declares, so a screen can show
// the form it is about to change rather than only what the cluster resolved.
func (g *GitOps) ClusterBackupPolicy(ctx context.Context) (*BackupPolicy, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepoRead(ctx); err != nil {
		return nil, err
	}
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	raw, err := os.ReadFile(filepath.Join(g.path, "clusters", cluster, "kernel", "claims", BackupPolicyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec BackupPolicy `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return &doc.Spec, nil
}

// writeTenantFile writes (or removes, when body is empty) a file beside a
// tenant's manifest and keeps the kustomization's resource list in step, as
// one commit. Both are only correct together.
func (g *GitOps) writeTenantFile(ctx context.Context, tenant, name, body, message string, meta Meta) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	manifest, err := g.tenantFile(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	dir := filepath.Dir(manifest)
	path := filepath.Join(dir, name)
	kustomizationPath := filepath.Join(dir, "kustomization.yaml")

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	kustomization, err := os.ReadFile(kustomizationPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}

	var listed string
	var kustomizationChanged bool
	if body == "" {
		if errors.Is(err, os.ErrNotExist) || len(existing) == 0 {
			return Result{Status: "unchanged"}, nil
		}
		listed, kustomizationChanged = removeResourceListed(string(kustomization), name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
	} else {
		listed, kustomizationChanged = ensureResourceListed(string(kustomization), name)
		if string(existing) == body && !kustomizationChanged {
			return Result{Status: "unchanged"}, nil
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return Result{}, err
		}
	}

	files := []string{path}
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
	if err := g.commitPaths(ctx, rels, message, meta); err != nil {
		return Result{}, err
	}
	return g.landed(ctx, "updated")
}

// renderBackupPolicy writes the object a reviewer reads in the commit.
func renderBackupPolicy(scope, tenant string, p BackupPolicy) string {
	var b strings.Builder
	b.WriteString("# Managed by the director: this ")
	if scope == "tenant" {
		b.WriteString("tenant's")
	} else {
		b.WriteString("cluster's")
	}
	b.WriteString(" backup policy, set in the\n")
	b.WriteString("# administration console by whoever the commit names. What is not stated\n")
	b.WriteString("# here is inherited: a tenant from the cluster, the cluster from the\n")
	b.WriteString("# platform's own storage and key.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: BackupPolicy\n")
	b.WriteString("metadata:\n")
	if scope == "tenant" {
		b.WriteString("  name: " + tenant + "\n")
	} else {
		b.WriteString("  name: default\n")
	}
	b.WriteString("spec:\n")
	b.WriteString("  scope: " + scope + "\n")
	if scope == "tenant" {
		b.WriteString("  tenant: " + tenant + "\n")
	}
	if d := p.Destination; d != nil && (d.Endpoint != "" || d.Bucket != "" || d.Region != "") {
		b.WriteString("  destination:\n")
		writeScalar(&b, "    endpoint", d.Endpoint)
		writeScalar(&b, "    bucket", d.Bucket)
		writeScalar(&b, "    region", d.Region)
	}
	writeScalar(&b, "  schedule", p.Schedule)
	if p.SuspendSchedule {
		b.WriteString("  suspendSchedule: true\n")
	}
	if r := p.Retention; r != nil {
		b.WriteString("  retention:\n")
		writeCount(&b, "    keepLast", r.KeepLast)
		writeCount(&b, "    keepDaily", r.KeepDaily)
		writeCount(&b, "    keepWeekly", r.KeepWeekly)
		writeCount(&b, "    keepMonthly", r.KeepMonthly)
		writeCount(&b, "    keepYearly", r.KeepYearly)
	}
	if e := p.Encryption; e != nil && len(e.Recipients) > 0 {
		b.WriteString("  encryption:\n")
		b.WriteString("    recipients:\n")
		for _, r := range e.Recipients {
			b.WriteString("    - " + quoteScalar(r) + "\n")
		}
	}
	if p.AllowTenantOverride != nil {
		fmt.Fprintf(&b, "  allowTenantOverride: %t\n", *p.AllowTenantOverride)
	}
	return b.String()
}

func writeScalar(b *strings.Builder, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString(key + ": " + quoteScalar(value) + "\n")
}

func writeCount(b *strings.Builder, key string, v int32) {
	if v <= 0 {
		return
	}
	fmt.Fprintf(b, "%s: %d\n", key, v)
}

// ensureResourceListed adds a file to the kustomization's resources list.
// Empty text is a tenant directory without a kustomization, and gets the one
// every tenant starts with.
func ensureResourceListed(text, file string) (string, bool) {
	if strings.TrimSpace(text) == "" {
		return tenantKustomization() + "- " + file + "\n", true
	}
	entry := "- " + file
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == entry {
			return text, false
		}
	}
	for i, line := range lines {
		if strings.TrimSpace(line) != "resources:" {
			continue
		}
		end := i + 1
		for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "-") {
			end++
		}
		out := append([]string{}, lines[:end]...)
		out = append(out, entry)
		out = append(out, lines[end:]...)
		return strings.Join(out, "\n") + "\n", true
	}
	return strings.Join(lines, "\n") + "\nresources:\n" + entry + "\n", true
}

// removeResourceListed takes a file back out of the resources list.
func removeResourceListed(text, file string) (string, bool) {
	entry := "- " + file
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	out := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		if strings.TrimSpace(line) == entry {
			removed = true
			continue
		}
		out = append(out, line)
	}
	if !removed {
		return text, false
	}
	return strings.Join(out, "\n") + "\n", true
}
