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
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// A privilege grant, as a commit.
//
// A profile may declare that it needs something the default posture refuses:
// an admission policy waived, outbound network beyond the tenant baseline,
// access to the Kubernetes API. Declaring it is asking (AD-5). Until somebody
// answers, the component's install waits -- it is not rejected, because the
// answer may still come, and it is not run without the privilege, because a
// component that quietly starts unprivileged is the failure the mechanism
// exists to prevent.
//
// The answer is a commit rather than an edit to a live object, for the same
// reason a resource plan is: an approval is the thing a reviewer will want to
// find two years later, and git is where it stays findable. The operator reads
// it from the Tenant and puts it on the Component, so no approval path
// anywhere holds cluster credentials.

// PrivilegesFile is the patch a tenant's grants are written to.
const PrivilegesFile = "privileges.yaml"

// PrivilegeGrant is one answer, shaped like the CRD's own entry because the
// director commits it rather than interpreting it.
//
// Times are strings here: this type is rendered into YAML and read back from
// it, and RFC 3339 is what the API server accepts for a metav1.Time. Parsing
// them into time.Time only to format them again would add a way for a
// round trip to change a value.
type PrivilegeGrant struct {
	// Install is the Component the grant is for.
	Install string `json:"install"`
	// Privilege is <kind>/<name>, naming one entry of the profile's request.
	Privilege string `json:"privilege"`
	// Approver is the subject of the token that approved, never a field a
	// caller may supply.
	Approver   string `json:"approver"`
	ApprovedAt string `json:"approvedAt"`
	Reason     string `json:"reason"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
}

// Key is what makes a grant unique: one answer per privilege per install.
func (g PrivilegeGrant) Key() string { return g.Install + " " + g.Privilege }

// TenantPrivileges reads a tenant's grants back, so a console can show what
// has been approved and by whom rather than only what is still pending.
func (g *GitOps) TenantPrivileges(ctx context.Context, tenant string) ([]PrivilegeGrant, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tenantPrivileges(ctx, tenant)
}

// tenantPrivileges is the read without the lock, for the grant and revoke
// paths that hold it across a read and a write.
func (g *GitOps) tenantPrivileges(ctx context.Context, tenant string) ([]PrivilegeGrant, error) {
	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), PrivilegesFile))
	if errors.Is(err, os.ErrNotExist) {
		// Nothing granted, which is a real answer and the usual one.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec struct {
			Privileges []PrivilegeGrant `json:"privileges"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Spec.Privileges, nil
}

// GrantPrivilege records one approval. Re-granting something already granted
// on the same terms is "unchanged" rather than an error: an approver who
// presses the button twice has not done anything wrong, and a second commit
// saying the same thing would only make the history harder to read.
//
// A grant whose terms differ -- a new reason, a new expiry -- REPLACES the
// old one, and the commit shows the replacement. That is deliberate: two
// grants for one privilege would leave the question of which applies.
func (g *GitOps) GrantPrivilege(ctx context.Context, tenant string, grant PrivilegeGrant, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !ValidName(grant.Install) {
		return Result{}, fmt.Errorf("%w: install %q", ErrInvalidName, grant.Install)
	}
	if grant.Privilege == "" || grant.Approver == "" || strings.TrimSpace(grant.Reason) == "" {
		return Result{}, fmt.Errorf("a grant needs a privilege, an approver and a reason")
	}
	if grant.ApprovedAt == "" {
		grant.ApprovedAt = time.Now().UTC().Format(time.RFC3339)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	have, err := g.tenantPrivileges(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	next := make([]PrivilegeGrant, 0, len(have)+1)
	replaced := false
	for _, h := range have {
		if h.Key() == grant.Key() {
			next = append(next, grant)
			replaced = true
			continue
		}
		next = append(next, h)
	}
	if !replaced {
		next = append(next, grant)
	}
	return g.writeTenantFileLocked(ctx, tenant, PrivilegesFile, renderPrivileges(tenant, next), listPatch,
		fmt.Sprintf("Grant %s to %s for tenant %s", grant.Privilege, grant.Install, tenant), meta)
}

// RevokePrivilege withdraws one. The component's next reconcile finds the
// privilege pending again and holds the install, which is the same state it
// was in before anybody approved -- a revocation takes something away rather
// than breaking something.
func (g *GitOps) RevokePrivilege(ctx context.Context, tenant, install, privilege string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	have, err := g.tenantPrivileges(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	want := PrivilegeGrant{Install: install, Privilege: privilege}.Key()
	next := make([]PrivilegeGrant, 0, len(have))
	for _, h := range have {
		if h.Key() == want {
			continue
		}
		next = append(next, h)
	}
	if len(next) == len(have) {
		return Result{Status: "unchanged"}, nil
	}
	// An empty list is the absence of the file, not a file declaring nothing:
	// a patch with an empty privileges list would have to mean "remove them
	// all", and writeTenantFile already spells absence as an empty body.
	body := ""
	if len(next) > 0 {
		body = renderPrivileges(tenant, next)
	}
	return g.writeTenantFileLocked(ctx, tenant, PrivilegesFile, body, listPatch,
		fmt.Sprintf("Revoke %s from %s for tenant %s", privilege, install, tenant), meta)
}

// renderPrivileges writes the patch a reviewer reads in the commit.
//
// Sorted by install then privilege, so that granting one thing shows as one
// added line in the diff rather than as a reordering of the whole list.
func renderPrivileges(tenant string, grants []PrivilegeGrant) string {
	sorted := make([]PrivilegeGrant, len(grants))
	copy(sorted, grants)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key() < sorted[j].Key() })

	var b strings.Builder
	b.WriteString("# Managed by the director: the privilege requests somebody approved for\n")
	b.WriteString("# this tenant. Each entry names the component it applies to, the request\n")
	b.WriteString("# it answers, who answered and why. The operator copies them onto the\n")
	b.WriteString("# Component; a component with an unanswered request waits rather than\n")
	b.WriteString("# starting without the privilege.\n")
	b.WriteString("#\n")
	b.WriteString("# Removing an entry withdraws the privilege. The component then holds\n")
	b.WriteString("# again, which is where it was before the approval.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  privileges:\n")
	for _, g := range sorted {
		b.WriteString("    - install: " + g.Install + "\n")
		b.WriteString("      privilege: " + g.Privilege + "\n")
		b.WriteString("      approver: " + g.Approver + "\n")
		b.WriteString("      approvedAt: " + g.ApprovedAt + "\n")
		b.WriteString("      reason: " + yamlScalar(g.Reason) + "\n")
		if g.ExpiresAt != "" {
			b.WriteString("      expiresAt: " + g.ExpiresAt + "\n")
		}
	}
	return b.String()
}

// yamlScalar quotes a value that would otherwise not survive the round trip.
// A reason is free text in somebody's own words, so it may hold a colon, a
// leading quote or a newline, and an unquoted scalar carrying any of those is
// a different document when it is read back.
//
// JSON, because YAML is a superset of it: a JSON string is always a valid
// double-quoted YAML scalar on ONE line. yaml.Marshal would be the obvious
// choice and is the wrong one -- it renders a multi-line string as a block
// scalar, whose continuation lines carry their own indentation, and pasting
// that after "reason: " at six spaces produces a document that either fails
// to parse or parses as something else.
func yamlScalar(s string) string {
	out, err := json.Marshal(s)
	if err != nil {
		// Marshalling a string cannot fail. Go's own quoting is a valid
		// double-quoted YAML scalar too, so the fallback is still correct.
		return fmt.Sprintf("%q", s)
	}
	return string(out)
}
