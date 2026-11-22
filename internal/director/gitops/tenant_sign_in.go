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
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant's own groups, and the sign-ins between tenants of this cluster, as
// commits.
//
// Declared state (Tenant spec.signInFrom, spec.signInTo), written by the
// director as a patch beside the tenant (sign-in.yaml) the way its languages
// are. The composition makes the client and the identity provider from it; nothing here
// holds a Keycloak credential, and a realm made again from git has them again.

// SignInFile is the patch.
const SignInFile = "sign-in.yaml"

// ErrInvalidSignIn is a sign-in or a group that cannot be one.
var ErrInvalidSignIn = errors.New("invalid sign-in")

// signInUser is the one position every tenant has that a sign-in gives.
const signInUser = "user"

type signInPatch struct {
	Spec struct {
		SignInFrom []gentianov1alpha1.TenantSignInFrom `json:"signInFrom"`
		SignInTo   []string                            `json:"signInTo"`
	} `json:"spec"`
}

// readTenantPatch reads one patch beside a tenant into doc; a patch that is
// not there leaves doc as it is. The caller holds the lock.
func (g *GitOps) readTenantPatch(ctx context.Context, tenant, name string, doc any) error {
	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(raw, doc); err != nil {
		return fmt.Errorf("read %s of tenant %s: %w", name, tenant, err)
	}
	return nil
}

// TenantSignIn reads what a tenant declares of sign-ins.
func (g *GitOps) TenantSignIn(ctx context.Context, tenant string) (from []gentianov1alpha1.TenantSignInFrom, to []string, err error) {
	if !gentianov1alpha1.ValidTenantName(tenant) {
		return nil, nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var doc signInPatch
	if err := g.readTenantPatch(ctx, tenant, SignInFile, &doc); err != nil {
		return nil, nil, err
	}
	return doc.Spec.SignInFrom, doc.Spec.SignInTo, nil
}

// SetTenantSignIn lets the people of source sign in to target, with the
// positions of target given, or ends it when remove is set.
// Two commits' worth in one: the target's signInFrom entry and the source's
// signInTo, each in its own tenant's patch, committed together so neither
// half lands without the other.
//
// A position is one target declares (spec.positions) or user. Refused: a source that is the target, a tenant that is not there.
func (g *GitOps) SetTenantSignIn(ctx context.Context, target, source string, positions []string, remove bool, meta Meta) (Result, error) {
	if !gentianov1alpha1.ValidTenantName(target) || !gentianov1alpha1.ValidTenantName(source) {
		return Result{}, fmt.Errorf("%w: tenants %q and %q", ErrInvalidName, target, source)
	}
	if target == source {
		return Result{}, fmt.Errorf("%w: a tenant's people already sign in to it", ErrInvalidSignIn)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Against the remote's latest word: what is read next is rewritten.
	for _, t := range []string{target, source} {
		if _, err := g.tenantFile(ctx, t); err != nil {
			return Result{}, err
		}
	}
	declared, err := g.tenantPositionsLocked(ctx, target)
	if err != nil {
		return Result{}, err
	}
	known := map[string]bool{signInUser: true}
	for _, p := range declared {
		known[p.Name] = true
	}
	var given []string
	for _, n := range positions {
		n = strings.TrimSpace(n)
		if n == "" || slices.Contains(given, n) {
			continue
		}
		if !known[n] {
			return Result{}, fmt.Errorf("%w: %s has no position %q a sign-in gives; it gives user or a position "+
				"the tenant declares", ErrInvalidSignIn, target, n)
		}
		given = append(given, n)
	}

	var tdoc, sdoc signInPatch
	if err := g.readTenantPatch(ctx, target, SignInFile, &tdoc); err != nil {
		return Result{}, err
	}
	if err := g.readTenantPatch(ctx, source, SignInFile, &sdoc); err != nil {
		return Result{}, err
	}
	from := slices.DeleteFunc(tdoc.Spec.SignInFrom, func(f gentianov1alpha1.TenantSignInFrom) bool { return f.Tenant == source })
	to := slices.DeleteFunc(sdoc.Spec.SignInTo, func(n string) bool { return n == target })
	if !remove {
		from = append(from, gentianov1alpha1.TenantSignInFrom{Tenant: source, Positions: given})
		to = append(to, target)
	}
	sort.Slice(from, func(i, j int) bool { return from[i].Tenant < from[j].Tenant })
	sort.Strings(to)
	tdoc.Spec.SignInFrom = from
	sdoc.Spec.SignInTo = to

	message := fmt.Sprintf("People of tenant %s sign in to tenant %s as %s", source, target, strings.Join(given, ", "))
	if remove {
		message = fmt.Sprintf("People of tenant %s no longer sign in to tenant %s", source, target)
	}
	return g.writeTenantFilesLocked(ctx, message, meta,
		tenantFileWrite{tenant: target, name: SignInFile, body: renderSignIn(target, tdoc), list: listPatch},
		tenantFileWrite{tenant: source, name: SignInFile, body: renderSignIn(source, sdoc), list: listPatch})
}

func writeTenantHead(b *strings.Builder, tenant string) {
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("spec:\n")
}

// renderSignIn is the patch a reviewer reads; empty when it declares nothing.
func renderSignIn(tenant string, doc signInPatch) string {
	if len(doc.Spec.SignInFrom) == 0 && len(doc.Spec.SignInTo) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Managed by the director: tenants of this cluster whose people sign in here\n")
	b.WriteString("# (signInFrom, with the groups they are given), and tenants this one's people\n")
	b.WriteString("# sign in to (signInTo). A sign-in takes both tenants' line.\n")
	writeTenantHead(&b, tenant)
	if len(doc.Spec.SignInFrom) > 0 {
		b.WriteString("  signInFrom:\n")
		for _, f := range doc.Spec.SignInFrom {
			b.WriteString("    - tenant: " + f.Tenant + "\n")
			if len(f.Positions) > 0 {
				b.WriteString("      positions: [" + strings.Join(f.Positions, ", ") + "]\n")
			}
		}
	}
	if len(doc.Spec.SignInTo) > 0 {
		b.WriteString("  signInTo: [" + strings.Join(doc.Spec.SignInTo, ", ") + "]\n")
	}
	return b.String()
}

// tenantFileWrite is one patch beside one tenant: written, or removed when
// body is empty.
type tenantFileWrite struct {
	tenant, name, body string
	// list is how the kustomization lists the file: a resource or a patch,
	// said every time (the zero value is a resource).
	list kustomizationList
}

// writeTenantFilesLocked writes several tenants' patches, and keeps each
// tenant's kustomization listing them, as one commit: a sign-in is two
// tenants' line, and a repository synced between them would carry one half.
// The caller holds the lock.
func (g *GitOps) writeTenantFilesLocked(ctx context.Context, message string, meta Meta, writes ...tenantFileWrite) (Result, error) {
	// Every tenant's directory first: resolving one brings the checkout to
	// the remote, which would take away a file already written for another.
	dirs := make([]string, len(writes))
	for i, w := range writes {
		manifest, err := g.tenantFile(ctx, w.tenant)
		if err != nil {
			return Result{}, err
		}
		dirs[i] = filepath.Dir(manifest)
	}
	var rels []string
	for i, w := range writes {
		dir := dirs[i]
		path := filepath.Join(dir, w.name)
		kustomizationPath := filepath.Join(dir, "kustomization.yaml")
		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		missing := errors.Is(err, os.ErrNotExist)
		kustomization, err := os.ReadFile(kustomizationPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
		var listed string
		var changedList, changedFile bool
		if w.body == "" {
			if missing {
				continue
			}
			listed, changedList = removeListed(string(kustomization), w.name, w.list)
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return Result{}, err
			}
			changedFile = true
		} else {
			listed, changedList = ensureListed(string(kustomization), w.name, w.list)
			if string(existing) != w.body {
				if err := os.WriteFile(path, []byte(w.body), 0o644); err != nil {
					return Result{}, err
				}
				changedFile = true
			}
		}
		if changedFile {
			rel, err := filepath.Rel(g.path, path)
			if err != nil {
				return Result{}, err
			}
			rels = append(rels, rel)
		}
		if changedList {
			if err := os.WriteFile(kustomizationPath, []byte(listed), 0o644); err != nil {
				return Result{}, err
			}
			rel, err := filepath.Rel(g.path, kustomizationPath)
			if err != nil {
				return Result{}, err
			}
			rels = append(rels, rel)
		}
	}
	if len(rels) == 0 {
		return Result{Status: "unchanged"}, nil
	}
	if err := g.commitPaths(ctx, rels, message, meta); err != nil {
		return Result{}, err
	}
	return g.landed(ctx, "updated")
}
