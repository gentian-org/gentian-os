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
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// What one app may consume from another, as a commit.
//
// A grant is declared state of the plainest kind: it says that this app, in
// this tenant, may use that much of that contract. It is the thing a binding
// is checked against, so it has to be reviewable — "who let the notes app
// read the whole drive, and when" is a question with an answer, and the
// answer is in the grant itself as well as in its commit: who granted it
// (from the caller's token, never from the request) and when.
//
// The grants are a patch of the Tenant (spec.grants, grants.yaml beside it),
// as its privilege approvals are, and the operator makes each the AppGrant
// of its app in the tenant's namespace. They were once AppGrant files of
// their own, which Argo CD could not apply: the project that syncs a
// tenant's directory writes to no tenant namespace. A write carries any such
// file over into the patch and removes it, in the same commit.

// GrantsFile is the patch a tenant's grants are written to.
const GrantsFile = "grants.yaml"

// AppGrant is what a caller states.
type AppGrant struct {
	Consume        []ConsumeGrant  `json:"consume,omitempty"`
	AllowConsumers []AllowConsumer `json:"allowConsumers,omitempty"`
	// Reason is optional: why, in the granter's words.
	Reason string `json:"reason,omitempty"`
}

// ConsumeGrant is one contract this app may consume, and how much of it.
type ConsumeGrant struct {
	Contract string   `json:"contract"`
	Granted  []string `json:"granted,omitempty"`
}

// AllowConsumer is one app permitted to consume from this one.
type AllowConsumer struct {
	App      string   `json:"app"`
	Contract string   `json:"contract"`
	Scope    []string `json:"scope,omitempty"`
}

// AppGrantFile is where one app's grant was written before grants were a
// patch of the Tenant; read only to carry it over.
func AppGrantFile(app string) string { return "grant-" + app + ".yaml" }

type grantsPatch struct {
	Spec struct {
		Grants []gentianov1alpha1.TenantAppGrant `json:"grants"`
	} `json:"spec"`
}

// SetAppGrant writes what one app may consume, with who granted it and when,
// and commits it.
func (g *GitOps) SetAppGrant(ctx context.Context, tenant, app string, grant AppGrant, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !ValidName(app) {
		return Result{}, fmt.Errorf("%w: app %q", ErrInvalidName, app)
	}
	if len(grant.Reason) > 2000 {
		return Result{}, fmt.Errorf("a grant's reason is at most 2000 characters")
	}
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	entry := gentianov1alpha1.TenantAppGrant{
		AppGrantSpec: appGrantSpec(app, grant),
		GrantedBy:    meta.Subject,
		GrantedAt:    &now,
		Reason:       strings.TrimSpace(grant.Reason),
	}
	return g.writeGrants(ctx, tenant, app, &entry,
		fmt.Sprintf("Set what %s may consume in tenant %s", app, tenant), meta)
}

// ClearAppGrant removes one, which withdraws everything it permitted.
func (g *GitOps) ClearAppGrant(ctx context.Context, tenant, app string, meta Meta) (Result, error) {
	if !ValidName(tenant) || !ValidName(app) {
		return Result{}, fmt.Errorf("%w: tenant %q app %q", ErrInvalidName, tenant, app)
	}
	return g.writeGrants(ctx, tenant, app, nil,
		fmt.Sprintf("Withdraw what %s may consume in tenant %s", app, tenant), meta)
}

// writeGrants puts entry in place of app's grant (or takes it away when nil),
// carries every old grant file over, and commits the patch and the removals
// together.
func (g *GitOps) writeGrants(ctx context.Context, tenant, app string, entry *gentianov1alpha1.TenantAppGrant, message string, meta Meta) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	manifest, err := g.tenantFile(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	dir := filepath.Dir(manifest)
	var doc grantsPatch
	if err := g.readTenantPatch(ctx, tenant, GrantsFile, &doc); err != nil {
		return Result{}, err
	}
	byApp := map[string]gentianov1alpha1.TenantAppGrant{}
	for _, e := range doc.Spec.Grants {
		byApp[e.App] = e
	}
	// The old files: each carried over where the patch has no entry yet.
	legacy, err := filepath.Glob(filepath.Join(dir, "grant-*.yaml"))
	if err != nil {
		return Result{}, err
	}
	var writes []tenantFileWrite
	for _, path := range legacy {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Result{}, err
		}
		var old struct {
			Spec gentianov1alpha1.AppGrantSpec `json:"spec"`
		}
		if err := yaml.Unmarshal(raw, &old); err != nil || !ValidName(old.Spec.App) {
			return Result{}, fmt.Errorf("read %s: not an app's grant", filepath.Base(path))
		}
		if _, ok := byApp[old.Spec.App]; !ok {
			byApp[old.Spec.App] = gentianov1alpha1.TenantAppGrant{AppGrantSpec: old.Spec}
		}
		writes = append(writes, tenantFileWrite{tenant: tenant, name: filepath.Base(path), list: listResource})
	}
	if entry != nil {
		byApp[app] = *entry
	} else {
		if _, ok := byApp[app]; !ok && len(writes) == 0 {
			return Result{Status: "unchanged"}, nil
		}
		delete(byApp, app)
	}
	apps := make([]string, 0, len(byApp))
	for a := range byApp {
		apps = append(apps, a)
	}
	sort.Strings(apps)
	doc.Spec.Grants = doc.Spec.Grants[:0]
	for _, a := range apps {
		doc.Spec.Grants = append(doc.Spec.Grants, byApp[a])
	}
	body, err := renderGrants(tenant, doc)
	if err != nil {
		return Result{}, err
	}
	writes = append([]tenantFileWrite{{tenant: tenant, name: GrantsFile, body: body, list: listPatch}}, writes...)
	if len(legacy) > 0 {
		message += " (grant files carried over into grants.yaml)"
	}
	return g.writeTenantFilesLocked(ctx, message, meta, writes...)
}

func appGrantSpec(app string, grant AppGrant) gentianov1alpha1.AppGrantSpec {
	spec := gentianov1alpha1.AppGrantSpec{App: app}
	for _, c := range grant.Consume {
		spec.Consume = append(spec.Consume, gentianov1alpha1.ConsumeGrantSpec{Contract: c.Contract, Granted: c.Granted})
	}
	for _, a := range grant.AllowConsumers {
		spec.AllowConsumers = append(spec.AllowConsumers, gentianov1alpha1.AllowConsumerSpec{App: a.App, Contract: a.Contract, Scope: a.Scope})
	}
	return spec
}

// renderGrants is the patch a reviewer reads; empty when there is no grant.
func renderGrants(tenant string, doc grantsPatch) (string, error) {
	if len(doc.Spec.Grants) == 0 {
		return "", nil
	}
	out, err := yaml.Marshal(map[string]any{
		"apiVersion": "gentianos.io/v1alpha1",
		"kind":       "Tenant",
		"metadata":   map[string]any{"name": tenant},
		"spec":       map[string]any{"grants": doc.Spec.Grants},
	})
	if err != nil {
		return "", err
	}
	return "# Managed by the director: what each app of this tenant may consume from\n" +
		"# the others, and which may consume from it, with who granted it and when.\n" +
		"# Set in the administration console by whoever may grant (can_grant); the\n" +
		"# operator makes each the AppGrant of its app in the tenant's namespace.\n" +
		string(out), nil
}

// ── What the platform permits to escape its default posture ─────────────────

// PlatformSecurityFile is where the cluster's waiver allowlist is written.
//
// Beside the claims, like the cluster's backup policy, because it is a thing
// the cluster declares about itself. It replaces the copy the operator chart
// used to create from its own values: one object with two writers is a sync
// conflict waiting for the first person who edits the wrong one.
const PlatformSecurityFile = "platform-security-policy.yaml"

// MacWaiver is one permitted escape: this profile, this policy, this scope.
type MacWaiver struct {
	Profile string `json:"profile"`
	Policy  string `json:"policy"`
	Scope   string `json:"scope"`
}

// ClusterRoleAllowance permits one of the platform's cluster roles for one
// profile on this cluster.
type ClusterRoleAllowance struct {
	Profile string `json:"profile"`
	Role    string `json:"role"`
}

// platformSecurityDoc is the whole object, as the file holds it. The two
// lists are written by two calls, and each carries the other across
// unchanged: a call that rendered only its own list would empty the other.
type platformSecurityDoc struct {
	Spec struct {
		AllowedMacWaivers   []MacWaiver            `json:"allowedMacWaivers"`
		AllowedClusterRoles []ClusterRoleAllowance `json:"allowedClusterRoles"`
	} `json:"spec"`
}

func (g *GitOps) platformSecurityDoc(ctx context.Context) (platformSecurityDoc, error) {
	var doc platformSecurityDoc
	_, err := g.readClaimsFile(ctx, PlatformSecurityFile, &doc)
	return doc, err
}

// SetPlatformSecurity writes the cluster's waiver allowlist and commits it.
//
// An allowlist and not a request: a profile asks for a waiver in its own
// catalogue entry, and this says which of those asks the cluster permits. The
// operator intersects the two, and with the grant on the install, so a waiver
// here for a profile that asks for nothing permits nothing, and is harmless
// rather than dangerous.
func (g *GitOps) SetPlatformSecurity(ctx context.Context, waivers []MacWaiver, meta Meta) (Result, error) {
	for _, w := range waivers {
		if strings.TrimSpace(w.Profile) == "" || strings.TrimSpace(w.Policy) == "" || strings.TrimSpace(w.Scope) == "" {
			return Result{}, fmt.Errorf("%w: a waiver needs a profile, a policy and a scope", ErrInvalidName)
		}
	}
	doc, err := g.platformSecurityDoc(ctx)
	if err != nil {
		return Result{}, err
	}
	return g.writeClaimsFile(ctx, PlatformSecurityFile, renderPlatformSecurity(waivers, doc.Spec.AllowedClusterRoles),
		"Set which MAC waivers this cluster permits", meta)
}

// SetPlatformClusterRoles writes which of the platform's cluster roles this
// cluster permits, per profile, and commits it.
//
// It permits and does not grant, and it cannot name a role into being: the
// operator binds a role only if the platform defines it, this permits it for
// the profile, and it was granted on the install.
func (g *GitOps) SetPlatformClusterRoles(ctx context.Context, roles []ClusterRoleAllowance, meta Meta) (Result, error) {
	for _, r := range roles {
		if !ValidName(r.Profile) || !ValidName(r.Role) {
			return Result{}, fmt.Errorf("%w: a permitted cluster role needs a profile and a role, each a lower-case name", ErrInvalidName)
		}
	}
	doc, err := g.platformSecurityDoc(ctx)
	if err != nil {
		return Result{}, err
	}
	return g.writeClaimsFile(ctx, PlatformSecurityFile, renderPlatformSecurity(doc.Spec.AllowedMacWaivers, roles),
		"Set which cluster roles this cluster permits", meta)
}

// PlatformSecurity reads back what the cluster declares, so a screen shows
// the list it is about to change rather than only the one in force.
func (g *GitOps) PlatformSecurity(ctx context.Context) ([]MacWaiver, error) {
	doc, err := g.platformSecurityDoc(ctx)
	return doc.Spec.AllowedMacWaivers, err
}

// PlatformClusterRoles reads back which cluster roles the cluster permits.
func (g *GitOps) PlatformClusterRoles(ctx context.Context) ([]ClusterRoleAllowance, error) {
	doc, err := g.platformSecurityDoc(ctx)
	return doc.Spec.AllowedClusterRoles, err
}

func renderPlatformSecurity(waivers []MacWaiver, roles []ClusterRoleAllowance) string {
	var b strings.Builder
	b.WriteString("# Managed by the director: which MAC waivers and which of the platform's\n")
	b.WriteString("# cluster roles this cluster permits, set by whoever the commit names.\n")
	b.WriteString("#\n")
	b.WriteString("# An allowlist, not a request and not a grant. A profile asks in its own\n")
	b.WriteString("# catalogue entry; an entry here for a profile that asks for nothing\n")
	b.WriteString("# permits nothing, and what is permitted takes effect only where it was\n")
	b.WriteString("# also granted on the install.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: PlatformSecurityPolicy\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: default\n")
	b.WriteString("spec:\n")
	if len(waivers) == 0 {
		b.WriteString("  # Nothing escapes the default posture.\n")
		b.WriteString("  allowedMacWaivers: []\n")
	} else {
		b.WriteString("  allowedMacWaivers:\n")
		for _, w := range waivers {
			b.WriteString("  - profile: " + quoteScalar(w.Profile) + "\n")
			b.WriteString("    policy: " + quoteScalar(w.Policy) + "\n")
			b.WriteString("    scope: " + quoteScalar(w.Scope) + "\n")
		}
	}
	if len(roles) > 0 {
		b.WriteString("  allowedClusterRoles:\n")
		for _, r := range roles {
			b.WriteString("  - profile: " + quoteScalar(r.Profile) + "\n")
			b.WriteString("    role: " + quoteScalar(r.Role) + "\n")
		}
	}
	return b.String()
}
