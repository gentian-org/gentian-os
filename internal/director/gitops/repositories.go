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
	"strings"

	"sigs.k8s.io/yaml"
)

// A repository, as a commit.
//
// Where a tenant or the cluster installs software from is configuration: it
// decides what may enter. So the address is declared here, in a file with an
// author, and Argo CD applies it. The password is not here and never was: it
// is a secret, the Repository names where in the vault it belongs, and
// setting it is the custodian's.
//
// The custodian used to create these objects in the cluster itself, which
// left the answer to "who pointed this tenant at that address, and when" in
// no commit at all.
//
// Two places, following who owns the repository:
//
//	the cluster's   clusters/<c>/kernel/claims/<name>-repository.yaml
//	a tenant's      clusters/<c>/tenants/<t>/repository-<name>.yaml
//
// The first is beside the repositories the installer declares and is synced
// as a directory. The second is listed under the tenant's kustomization as a
// resource, which is what makes Argo CD apply it and prune it.
//
// A name is one object in the cluster whoever declares it, so a name is
// taken by whoever declared it first, and to anybody else it does not exist:
// answering "taken" would tell one tenant what another has.

// Repository roles: what losing one costs.
//
// apps is additive: a private catalogue beside the cluster's, and removing it
// removes those apps. deployments is a source of truth: repointing it changes
// what everything reconciles from, which is why declaring one is confirmed
// even when it is new.
const (
	RepositoryRoleApps        = "apps"
	RepositoryRoleDeployments = "deployments"
)

// RepositoryDeclaration is what a caller states about a repository. There is
// no field here that could carry a credential.
type RepositoryDeclaration struct {
	Role     string `json:"role"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Branch   string `json:"branch,omitempty"`
	Writable bool   `json:"writable,omitempty"`
	// Confirm must repeat the repository's name for any change that is not
	// purely additive. A confirmation enforced only on a screen is one a
	// script skips.
	Confirm string `json:"confirm,omitempty"`
}

// ErrRepositoryNotFound is a repository that is not this owner's to change:
// absent, or declared by somebody else.
var ErrRepositoryNotFound = errors.New("no such repository")

// ErrRepositoryNotRewritable is a repository declared in a file together
// with other objects, which this cannot rewrite without touching them.
var ErrRepositoryNotRewritable = errors.New("the repository is declared in a file that holds other objects as well; change it there")

// ConfirmationRequired is a change that is refused until the caller repeats
// the repository's name. Reason says why it is dangerous.
type ConfirmationRequired struct {
	Name   string
	Reason string
}

func (e *ConfirmationRequired) Error() string { return e.Reason }

// RepositoryResult is what declaring one answers.
type RepositoryResult struct {
	Result
	// Created is true when nothing declared this name before.
	Created bool
	// Tenant is the owner; empty for the cluster's.
	Tenant string
}

// CheckRepositoryDeclaration says what is wrong with a declaration, or nil.
func CheckRepositoryDeclaration(name string, d *RepositoryDeclaration) error {
	if !ValidName(name) {
		return fmt.Errorf("a repository's name is lower-case letters, digits and hyphens")
	}
	if d.URL == "" || strings.TrimSpace(d.URL) != d.URL || strings.ContainsAny(d.URL, "\n\r") {
		return fmt.Errorf("url is required and must not have leading or trailing whitespace")
	}
	switch d.Type {
	case "git", "oci":
	default:
		return fmt.Errorf("type must be git or oci, got %q", d.Type)
	}
	switch d.Role {
	case RepositoryRoleApps, RepositoryRoleDeployments:
	default:
		return fmt.Errorf("role must be %s or %s, got %q", RepositoryRoleApps, RepositoryRoleDeployments, d.Role)
	}
	if d.Role == RepositoryRoleDeployments && d.Type != "git" {
		return fmt.Errorf("a deployments repository must be git")
	}
	if strings.ContainsAny(d.Branch, " \t\n\r") {
		return fmt.Errorf("branch must not contain whitespace")
	}
	return nil
}

// repositoryNeedsConfirmation returns why a declaration is dangerous, or "".
//
// Adding an apps repository is additive and needs no ceremony. Everything
// else changes where something already running reconciles from, and a
// confirmation that is easy to click through is not a confirmation.
func repositoryNeedsConfirmation(existingURL string, found bool, d *RepositoryDeclaration) string {
	if d.Role == RepositoryRoleDeployments {
		// Even when new: pointing deployments somewhere else redirects
		// everything Argo CD reconciles from it.
		return "this repository is the source of truth for the tenant's deployments; " +
			"changing it redirects everything reconciled from it"
	}
	if !found {
		return ""
	}
	if existingURL != "" && existingURL != d.URL {
		return fmt.Sprintf("this replaces the existing source %q, and apps installed from it "+
			"will resolve against the new one", existingURL)
	}
	return ""
}

// RepositoryVaultPath is where a repository's credential belongs. Derived,
// never taken from the caller: a tenant's stays inside the prefix that
// tenant's credentials live under, so a declaration cannot name somebody
// else's secret as its own.
func RepositoryVaultPath(tenant, name string) string {
	if tenant == "" {
		return "gentian-os/kernel/repositories/" + name
	}
	return fmt.Sprintf("gentian-os/tenants/%s/repositories/%s", tenant, name)
}

// RepositoryCredentialName is the credential requirement the composition
// emits for a repository, which is what the custodian sets the password of.
func RepositoryCredentialName(name string) string { return "repository-" + name }

func clusterRepositoryFile(name string) string { return name + "-repository.yaml" }
func tenantRepositoryFile(name string) string  { return "repository-" + name + ".yaml" }

// declaredRepository is one Repository found in the checkout.
type declaredRepository struct {
	path string
	// tenantDir is the tenant whose directory holds it; empty for the
	// cluster's claims.
	tenantDir string
	// alone is false when the file holds other objects too.
	alone bool
	doc   map[string]any
}

func (d *declaredRepository) specTenant() string {
	spec, _ := d.doc["spec"].(map[string]any)
	t, _ := spec["tenant"].(string)
	return t
}

func (d *declaredRepository) url() string {
	spec, _ := d.doc["spec"].(map[string]any)
	endpoints, _ := spec["endpoints"].(map[string]any)
	u, _ := endpoints["inCluster"].(string)
	return u
}

// declaredRepositories finds every declaration of one name, anywhere Argo CD
// would apply it from: the cluster's claims and each tenant's directory. More
// than one is already a conflict, and the caller refuses to add to it.
func (g *GitOps) declaredRepositories(name string) ([]declaredRepository, error) {
	cluster := g.cluster
	if cluster == "" {
		cluster = "default-cluster"
	}
	var found []declaredRepository
	scan := func(dir, tenantDir string) error {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml")) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var docs []map[string]any
			for _, part := range strings.Split("\n"+string(raw), "\n---") {
				var doc map[string]any
				if yaml.Unmarshal([]byte(part), &doc) != nil || len(doc) == 0 {
					continue
				}
				docs = append(docs, doc)
			}
			for _, doc := range docs {
				if kind, _ := doc["kind"].(string); kind != "Repository" {
					continue
				}
				meta, _ := doc["metadata"].(map[string]any)
				if n, _ := meta["name"].(string); n != name {
					continue
				}
				found = append(found, declaredRepository{path: path, tenantDir: tenantDir, alone: len(docs) == 1, doc: doc})
			}
		}
		return nil
	}
	if err := scan(g.claimsDir(), ""); err != nil {
		return nil, err
	}
	tenantsRoot := filepath.Join(g.path, "clusters", cluster, "tenants")
	tenants, err := os.ReadDir(tenantsRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	names := make([]string, 0, len(tenants))
	for _, t := range tenants {
		if t.IsDir() {
			names = append(names, t.Name())
		}
	}
	sort.Strings(names)
	for _, t := range names {
		if err := scan(filepath.Join(tenantsRoot, t), t); err != nil {
			return nil, err
		}
	}
	return found, nil
}

// ownRepository picks the declaration that is owner's to change out of every
// declaration of a name. A name declared by anybody else, or by more than
// one, is not this owner's and is answered as absent.
func ownRepository(found []declaredRepository, owner string) (*declaredRepository, error) {
	if len(found) == 0 {
		return nil, nil
	}
	if len(found) > 1 || found[0].tenantDir != owner || found[0].specTenant() != owner {
		return nil, ErrRepositoryNotFound
	}
	if !found[0].alone {
		return nil, ErrRepositoryNotRewritable
	}
	return &found[0], nil
}

// DeclareRepository writes a repository's declaration and commits it. tenant
// is its owner; empty is the cluster.
func (g *GitOps) DeclareRepository(ctx context.Context, tenant, name string, d RepositoryDeclaration, meta Meta) (RepositoryResult, error) {
	if tenant != "" && !ValidName(tenant) {
		return RepositoryResult{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !ValidName(name) {
		return RepositoryResult{}, fmt.Errorf("%w: repository %q", ErrInvalidName, name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return RepositoryResult{}, err
	}
	if tenant != "" {
		// Resolved first: an unknown tenant is its own answer, and a file
		// for one would be applied by nothing.
		if _, err := g.tenantFile(ctx, tenant); err != nil {
			return RepositoryResult{}, err
		}
	}
	found, err := g.declaredRepositories(name)
	if err != nil {
		return RepositoryResult{}, err
	}
	existing, err := ownRepository(found, tenant)
	if err != nil {
		return RepositoryResult{}, err
	}
	existingURL := ""
	if existing != nil {
		existingURL = existing.url()
	}
	if reason := repositoryNeedsConfirmation(existingURL, existing != nil, &d); reason != "" && d.Confirm != name {
		return RepositoryResult{}, &ConfirmationRequired{Name: name, Reason: reason}
	}

	out := RepositoryResult{Created: existing == nil, Tenant: tenant}
	doc := map[string]any{
		"apiVersion": "gentianos.io/v1alpha1",
		"kind":       "Repository",
		"metadata":   map[string]any{"name": name},
	}
	if existing != nil {
		doc = existing.doc
	}
	body, err := renderRepository(doc, tenant, name, d)
	if err != nil {
		return RepositoryResult{}, err
	}
	who := "the cluster"
	if tenant != "" {
		who = "tenant " + tenant
	}
	message := fmt.Sprintf("Declare repository %s for %s", name, who)

	if tenant != "" {
		file := tenantRepositoryFile(name)
		if existing != nil {
			file = filepath.Base(existing.path)
		}
		res, err := g.writeTenantFileLocked(ctx, tenant, file, body, listResource, message, meta)
		out.Result = res
		return out, err
	}
	path := filepath.Join(g.claimsDir(), clusterRepositoryFile(name))
	if existing != nil {
		path = existing.path
	}
	res, err := g.writeFileLocked(ctx, path, body, message, meta)
	out.Result = res
	return out, err
}

// RemoveRepository stops declaring a repository and commits that. confirm
// must repeat its name: removal is always destructive, because the apps the
// repository carried stop reconciling.
func (g *GitOps) RemoveRepository(ctx context.Context, tenant, name, confirm string, meta Meta) (Result, error) {
	if tenant != "" && !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !ValidName(name) {
		return Result{}, fmt.Errorf("%w: repository %q", ErrInvalidName, name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	if tenant != "" {
		if _, err := g.tenantFile(ctx, tenant); err != nil {
			return Result{}, err
		}
	}
	found, err := g.declaredRepositories(name)
	if err != nil {
		return Result{}, err
	}
	existing, err := ownRepository(found, tenant)
	if err != nil {
		return Result{}, err
	}
	if existing == nil {
		return Result{}, ErrRepositoryNotFound
	}
	if confirm != name {
		return Result{}, &ConfirmationRequired{Name: name,
			Reason: fmt.Sprintf("removing %q stops every app it provides from reconciling", name)}
	}
	who := "the cluster"
	if tenant != "" {
		who = "tenant " + tenant
	}
	message := fmt.Sprintf("Remove repository %s of %s", name, who)
	if tenant != "" {
		return g.writeTenantFileLocked(ctx, tenant, filepath.Base(existing.path), "", listResource, message, meta)
	}
	return g.writeFileLocked(ctx, existing.path, "", message, meta)
}

// writeFileLocked writes one file of the checkout, or removes it when body is
// empty, and commits it. The caller holds the lock.
func (g *GitOps) writeFileLocked(ctx context.Context, path, body, message string, meta Meta) (Result, error) {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	switch {
	case body == "" && errors.Is(err, os.ErrNotExist):
		return Result{Status: "unchanged"}, nil
	case body == "":
		if err := os.Remove(path); err != nil {
			return Result{}, err
		}
	case string(existing) == body:
		return Result{Status: "unchanged"}, nil
	default:
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return Result{}, err
		}
	}
	rel, err := filepath.Rel(g.path, path)
	if err != nil {
		return Result{}, err
	}
	if err := g.commitPaths(ctx, []string{rel}, message, meta); err != nil {
		return Result{}, err
	}
	return g.landed(ctx, "updated")
}

// renderRepository states d on doc and writes the object a reviewer reads in
// the commit.
//
// A declaration that exists is changed, not replaced: what this API cannot
// express -- how the credential authenticates and is validated, a second
// address for the install host, which namespaces receive a pull secret -- is
// kept as it was. The installer's own repositories carry such fields, and a
// change of address must not cost the deployments repository the settings
// its push credential is read with.
func renderRepository(doc map[string]any, tenant, name string, d RepositoryDeclaration) (string, error) {
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	spec["type"] = d.Type
	spec["role"] = d.Role
	spec["writable"] = d.Writable
	endpoints, _ := spec["endpoints"].(map[string]any)
	if endpoints == nil {
		endpoints = map[string]any{}
	}
	endpoints["inCluster"] = d.URL
	spec["endpoints"] = endpoints
	if d.Branch != "" {
		spec["branch"] = d.Branch
	} else {
		delete(spec, "branch")
	}
	if tenant != "" {
		spec["tenant"] = tenant
	} else {
		delete(spec, "tenant")
	}
	// The credential is declared, never supplied: the composition emits a
	// CredentialRequirement from this, whose scope follows spec.tenant, and
	// the value is set at the custodian.
	if _, ok := spec["credential"].(map[string]any); !ok {
		spec["credential"] = map[string]any{
			"displayName": fmt.Sprintf("Credentials for %s", name),
			"vaultPath":   RepositoryVaultPath(tenant, name),
		}
	}
	doc["spec"] = spec
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Managed by the director: where ")
	if tenant != "" {
		b.WriteString("this tenant installs")
	} else {
		b.WriteString("the cluster installs")
	}
	b.WriteString(" software from, declared\n")
	b.WriteString("# by whoever the commit names. The address is here; the password is in the\n")
	b.WriteString("# vault, under the path below, and is set at the custodian.\n")
	b.Write(raw)
	return b.String(), nil
}
