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
	"regexp"
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant's own groups, and the sign-ins between tenants of this cluster, as
// commits.
//
// Both are declared state (Tenant spec.groups, spec.signInFrom,
// spec.signInTo), written by the director as patches beside the tenant
// (groups.yaml, sign-in.yaml) the way its languages are. The composition makes
// the groups, the client and the identity provider from them; nothing here
// holds a Keycloak credential, and a realm made again from git has them again.

// GroupsFile and SignInFile are the patches.
const (
	GroupsFile = "groups.yaml"
	SignInFile = "sign-in.yaml"
)

// ErrInvalidSignIn is a sign-in or a group that cannot be one.
var ErrInvalidSignIn = errors.New("invalid sign-in")

var groupName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// signInMembers is the one platform group a sign-in gives.
const signInMembers = "members"

type groupsPatch struct {
	Spec struct {
		Groups []gentianov1alpha1.TenantGroup `json:"groups"`
	} `json:"spec"`
}

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

// TenantGroups reads a tenant's declared groups back.
func (g *GitOps) TenantGroups(ctx context.Context, tenant string) ([]string, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var doc groupsPatch
	if err := g.readTenantPatch(ctx, tenant, GroupsFile, &doc); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.Spec.Groups))
	for _, gr := range doc.Spec.Groups {
		out = append(out, gr.Name)
	}
	return out, nil
}

// SetTenantGroups commits a tenant's declared groups; none removes the patch.
// A name the platform's own groups take (gentian...) or that is not a label
// is refused.
func (g *GitOps) SetTenantGroups(ctx context.Context, tenant string, groups []string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	clean, err := cleanGroupNames(groups)
	if err != nil {
		return Result{}, err
	}
	body := ""
	if len(clean) > 0 {
		var b strings.Builder
		b.WriteString("# Managed by the director: groups of this tenant's realm, declared here\n")
		b.WriteString("# rather than made in a console. The platform makes them; people are put in\n")
		b.WriteString("# them in the Admin Console, which neither renames nor deletes them.\n")
		writeTenantHead(&b, tenant)
		b.WriteString("  groups:\n")
		for _, n := range clean {
			b.WriteString("    - name: " + n + "\n")
		}
		body = b.String()
	}
	return g.writeTenantFile(ctx, tenant, GroupsFile, body, listPatch,
		fmt.Sprintf("Tenant %s: groups %s", tenant, strings.Join(clean, ", ")), meta)
}

func cleanGroupNames(groups []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, n := range groups {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		if !groupName.MatchString(n) || strings.HasPrefix(n, "gentian") || n == signInMembers {
			return nil, fmt.Errorf("%w: %q is not a name for a tenant's own group (lower-case letters, digits and "+
				"hyphens; not gentian..., not members)", ErrInvalidSignIn, n)
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// TenantSignIn reads what a tenant declares of sign-ins.
func (g *GitOps) TenantSignIn(ctx context.Context, tenant string) (from []gentianov1alpha1.TenantSignInFrom, to []string, err error) {
	if !ValidName(tenant) {
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
// groups of target given, or ends it when groups is nil and remove is set.
// Two commits' worth in one: the target's signInFrom entry and the source's
// signInTo, each in its own tenant's patch, committed together so neither
// half lands without the other.
//
// A group is one target declares (spec.groups, in its groups.yaml) or
// members. Refused: a source that is the target, a tenant that is not there.
func (g *GitOps) SetTenantSignIn(ctx context.Context, target, source string, groups []string, remove bool, meta Meta) (Result, error) {
	if !ValidName(target) || !ValidName(source) {
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
	var declared groupsPatch
	if err := g.readTenantPatch(ctx, target, GroupsFile, &declared); err != nil {
		return Result{}, err
	}
	known := map[string]bool{signInMembers: true}
	for _, gr := range declared.Spec.Groups {
		known[gr.Name] = true
	}
	var given []string
	for _, n := range groups {
		n = strings.TrimSpace(n)
		if n == "" || slices.Contains(given, n) {
			continue
		}
		if !known[n] {
			return Result{}, fmt.Errorf("%w: %s declares no group %q; a sign-in gives members or a group of "+
				"spec.groups (kubectl gentian tenants groups %s ...)", ErrInvalidSignIn, target, n, target)
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
		from = append(from, gentianov1alpha1.TenantSignInFrom{Tenant: source, Groups: given})
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
			if len(f.Groups) > 0 {
				b.WriteString("      groups: [" + strings.Join(f.Groups, ", ") + "]\n")
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
