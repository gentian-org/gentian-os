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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A bundle source, as a commit.
//
// Storage outside the cluster that bundles are imported from is declared the
// way the cluster's backup destination is: its address in a file of the
// cluster's claims directory, with an author, applied by Argo CD as a
// BundleSource. Its keys are not here and cannot be: the operator derives a
// credential requirement from the source's name, and the custodian stores the
// keys in the vault (internal/backup.SourceVaultPath).
//
//	clusters/<c>/kernel/claims/bundle-source-<name>.yaml
//
// A tenant's own store, which its exports are written to, is declared the
// same way in the tenant's directory, by its administrator, as the
// BundleSource <tenant>.<label> with spec.tenant set; its keys are set at the
// custodian in the tenant's part of the vault.
//
//	clusters/<c>/tenants/<t>/bundle-source-<label>.yaml
//
// One file per source, rather than a list in one file: declaring one and
// removing one are each a commit that touches nothing else.

// BundleSourceDeclaration is what a caller states about a source. There is
// no field that could carry a credential.
type BundleSourceDeclaration struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	Region          string `json:"region,omitempty"`
	AddressingStyle string `json:"addressingStyle,omitempty"`
	Description     string `json:"description,omitempty"`
}

// DeclaredBundleSource is one source as git declares it.
type DeclaredBundleSource struct {
	Name string `json:"name"`
	BundleSourceDeclaration
}

// ErrInvalidBundleSource is a declaration the BundleSource definition would
// refuse, told the caller before it is committed.
var ErrInvalidBundleSource = errors.New("invalid bundle source")

// ErrBundleSourceNotFound is a source nothing declares.
var ErrBundleSourceNotFound = errors.New("no such bundle source")

var (
	bundleSourceName   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,46}[a-z0-9])?$`)
	bundleSourceBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	bundleSourcePrefix = regexp.MustCompile(`^$|^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*/?$`)
)

// CheckBundleSource says what is wrong with a declaration, or nil.
func CheckBundleSource(name string, d *BundleSourceDeclaration) error {
	if !bundleSourceName.MatchString(name) {
		return fmt.Errorf("%w: a source's name is lower-case letters, digits and hyphens, 48 at most", ErrInvalidBundleSource)
	}
	u, err := url.Parse(d.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		strings.ContainsAny(d.Endpoint, " \t\r\n") || len(d.Endpoint) > 2048 {
		return fmt.Errorf("%w: endpoint is the storage's S3 address, https://host[:port], without a login", ErrInvalidBundleSource)
	}
	if !bundleSourceBucket.MatchString(d.Bucket) {
		return fmt.Errorf("%w: bucket %q is not a bucket's name", ErrInvalidBundleSource, d.Bucket)
	}
	if !bundleSourcePrefix.MatchString(d.Prefix) || len(d.Prefix) > 512 {
		return fmt.Errorf("%w: prefix is a path below the bucket, of letters, digits, dots, dashes and underscores, no segment beginning with a dot", ErrInvalidBundleSource)
	}
	switch d.AddressingStyle {
	case "", string(gentianov1alpha1.S3AddressingPath), string(gentianov1alpha1.S3AddressingVirtualHost):
	default:
		return fmt.Errorf("%w: addressingStyle is path or virtualHost", ErrInvalidBundleSource)
	}
	if len(d.Region) > 64 || strings.ContainsAny(d.Region, " \t\r\n") {
		return fmt.Errorf("%w: region is one word", ErrInvalidBundleSource)
	}
	if len(d.Description) > 256 || strings.ContainsAny(d.Description, "\r\n") {
		return fmt.Errorf("%w: description is one line, 256 characters at most", ErrInvalidBundleSource)
	}
	return nil
}

func bundleSourceFile(name string) string { return "bundle-source-" + name + ".yaml" }

// DeclareBundleSource writes one source and commits it.
func (g *GitOps) DeclareBundleSource(ctx context.Context, name string, d BundleSourceDeclaration, meta Meta) (Result, error) {
	d.Endpoint = strings.TrimSpace(d.Endpoint)
	d.Description = strings.TrimSpace(d.Description)
	if err := CheckBundleSource(name, &d); err != nil {
		return Result{}, err
	}
	return g.writeClaimsFile(ctx, bundleSourceFile(name), renderBundleSource(name, "", d),
		fmt.Sprintf("Declare bundle source %s: %s, bucket %s", name, d.Endpoint, d.Bucket), meta)
}

// TenantStoreName is the BundleSource a tenant's store is declared as, or an
// error saying why the label cannot be one: the tenant, a dot, and the label.
// No tenant's name and no label can hold a dot, so two tenants' stores, and a
// tenant's and the cluster's, can never share a name.
func TenantStoreName(tenant, label string) (string, error) {
	if !ValidName(tenant) {
		return "", fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !bundleSourceName.MatchString(label) {
		return "", fmt.Errorf("%w: a store's name is lower-case letters, digits and hyphens, 48 at most", ErrInvalidBundleSource)
	}
	name := tenant + "." + label
	if len(name) > 63 {
		return "", fmt.Errorf("%w: the tenant's name and the store's together are 62 characters at most", ErrInvalidBundleSource)
	}
	return name, nil
}

// DeclareTenantBundleSource writes one of a tenant's own stores and commits it.
func (g *GitOps) DeclareTenantBundleSource(ctx context.Context, tenant, label string, d BundleSourceDeclaration, meta Meta) (Result, error) {
	name, err := TenantStoreName(tenant, label)
	if err != nil {
		return Result{}, err
	}
	d.Endpoint = strings.TrimSpace(d.Endpoint)
	d.Description = strings.TrimSpace(d.Description)
	if err := CheckBundleSource(label, &d); err != nil {
		return Result{}, err
	}
	return g.writeTenantFile(ctx, tenant, bundleSourceFile(label), renderBundleSource(name, tenant, d), listResource,
		fmt.Sprintf("Declare store %s of tenant %s: %s, bucket %s", label, tenant, d.Endpoint, d.Bucket), meta)
}

// RemoveTenantBundleSource stops declaring one of a tenant's stores. The
// tenant's directory is synced with pruning, so the cluster deletes it, and
// its keys go with the requirement made for it. What was written there stays.
func (g *GitOps) RemoveTenantBundleSource(ctx context.Context, tenant, label string, meta Meta) (Result, error) {
	if _, err := TenantStoreName(tenant, label); err != nil {
		return Result{}, err
	}
	res, err := g.writeTenantFile(ctx, tenant, bundleSourceFile(label), "", listResource,
		fmt.Sprintf("Remove store %s of tenant %s", label, tenant), meta)
	if err == nil && res.Status == "unchanged" {
		return Result{}, ErrBundleSourceNotFound
	}
	return res, err
}

// RemoveBundleSource stops declaring a source. What was fetched from it
// stays; its keys go with the requirement the operator made for it.
func (g *GitOps) RemoveBundleSource(ctx context.Context, name string, meta Meta) (Result, error) {
	if !bundleSourceName.MatchString(name) {
		return Result{}, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepo(ctx); err != nil {
		return Result{}, err
	}
	path := filepath.Join(g.claimsDir(), bundleSourceFile(name))
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return Result{}, ErrBundleSourceNotFound
	}
	return g.writeFileLocked(ctx, path, "", fmt.Sprintf("Remove bundle source %s", name), meta)
}

// BundleSources reads every source git declares.
func (g *GitOps) BundleSources(ctx context.Context) ([]DeclaredBundleSource, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.ensureRepoRead(ctx); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(g.claimsDir())
	if errors.Is(err, os.ErrNotExist) {
		return []DeclaredBundleSource{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []DeclaredBundleSource{}
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), "bundle-source-")
		if e.IsDir() || !ok || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(g.claimsDir(), e.Name()))
		if err != nil {
			return nil, err
		}
		var doc struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec BundleSourceDeclaration `json:"spec"`
		}
		if yaml.Unmarshal(raw, &doc) != nil || doc.Metadata.Name == "" {
			continue
		}
		out = append(out, DeclaredBundleSource{Name: doc.Metadata.Name, BundleSourceDeclaration: doc.Spec})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// renderBundleSource writes the object a reviewer reads in the commit. A
// tenant's own store carries the tenant; the cluster's carries none.
func renderBundleSource(name, tenant string, d BundleSourceDeclaration) string {
	var b strings.Builder
	if tenant == "" {
		b.WriteString("# Managed by the director: storage outside the cluster that bundles are\n")
		b.WriteString("# imported from, declared by whoever the commit names. Its keys are not\n")
	} else {
		b.WriteString("# Managed by the director: storage outside the cluster that this tenant's\n")
		b.WriteString("# exports are written to, declared by whoever the commit names. Its keys are not\n")
	}
	b.WriteString("# here: they are set at the custodian, as bundle-source-" + name + ".\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: BundleSource\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + name + "\n")
	b.WriteString("spec:\n")
	// Quoted always: a prefix or a region of digits alone is a string, and
	// left bare YAML would read it as a number the definition refuses.
	for _, f := range []struct{ key, value string }{
		{"endpoint", d.Endpoint}, {"bucket", d.Bucket}, {"prefix", d.Prefix},
		{"region", d.Region}, {"addressingStyle", d.AddressingStyle}, {"tenant", tenant},
		{"description", d.Description},
	} {
		if f.value != "" {
			b.WriteString("  " + f.key + ": " + strconv.Quote(f.value) + "\n")
		}
	}
	return b.String()
}
