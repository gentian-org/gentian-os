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
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// Who on the cluster holds which cluster role, and whether a tenant lets the
// platform's administrators sign in to it, as git declares them.
//
// On the Cluster claim, the role holders (spec.platformAccess.roleHolders):
// people of the platform tenant and administrators of the cluster's other
// tenants, each by their subject in their tenant's realm, holding a role the
// cluster's administrator gave them, with the reason and, if it ends, when.
// The operator writes each entry into the authorization graph as the
// account of that tenant's realm. On a Tenant's manifest, whether the
// platform's administrators may sign in to it
// (spec.platformAccess.adminsEnter), which the tenant's own administrators
// set; its realm then composes the identity provider "kernel" for them.

// RoleHolder is one entry of spec.platformAccess.roleHolders.
type RoleHolder struct {
	// Role is the claim's name for the role: one of authz.HolderRoles.
	Role string `json:"role"`
	// Scope is how far it reaches: "cluster", or "tenant:<t>" for the
	// perimeter approver of one tenant (authz.HolderScope).
	Scope string `json:"scope,omitempty"`
	// Tenant is the tenant whose realm holds the account.
	Tenant string `json:"tenant"`
	// Subject is the account's id in that realm, which outlives a changed
	// address.
	Subject string `json:"subject"`
	// Username and Email say who it is to a person reading the claim.
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	// Reason is why the cluster's administrator gave the role.
	Reason string `json:"reason,omitempty"`
	// ExpiresAt, RFC 3339, is when the role ends; empty when it does not.
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// ErrInvalidRoleHolder is an entry that cannot be written.
var ErrInvalidRoleHolder = errors.New("invalid role holder")

// ErrPlatformAccessPlatformTenant is the tenant's switch asked for the
// platform tenant, whose administrators are the platform's already.
var ErrPlatformAccessPlatformTenant = errors.New(
	"this is the platform tenant: its administrators are the platform's own, " +
		"who are in it already. There is nothing to switch on")

// holderSubject is what a subject may look like: Keycloak's user ids are
// UUIDs.
var holderSubject = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type roleHoldersClaim struct {
	Spec struct {
		PlatformAccess struct {
			RoleHolders []RoleHolder `json:"roleHolders"`
		} `json:"platformAccess"`
	} `json:"spec"`
}

func parseRoleHolders(text string) ([]RoleHolder, error) {
	var doc roleHoldersClaim
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("parse cluster claim: %w", err)
	}
	return sortedHolders(doc.Spec.PlatformAccess.RoleHolders), nil
}

func sortedHolders(list []RoleHolder) []RoleHolder {
	out := append([]RoleHolder(nil), list...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		if a.Tenant != b.Tenant {
			return a.Tenant < b.Tenant
		}
		return a.Subject < b.Subject
	})
	return out
}

// RoleHolders reads who the claim names.
func (g *GitOps) RoleHolders(ctx context.Context) ([]RoleHolder, error) {
	var doc roleHoldersClaim
	if err := g.readClusterClaim(ctx, &doc); err != nil {
		return nil, err
	}
	out := sortedHolders(doc.Spec.PlatformAccess.RoleHolders)
	if out == nil {
		out = []RoleHolder{}
	}
	return out, nil
}

// HolderRoleNames are the roles an entry may name, sorted.
func HolderRoleNames() []string {
	out := make([]string, 0, len(authz.HolderRoles))
	for r := range authz.HolderRoles {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func validHolderKey(role, tenant, subject string) error {
	if _, ok := authz.HolderRoles[role]; !ok {
		return fmt.Errorf("%w: role %q is not one of %s", ErrInvalidRoleHolder, role, strings.Join(HolderRoleNames(), ", "))
	}
	if !ValidName(tenant) {
		return fmt.Errorf("%w: tenant %q is not a name", ErrInvalidRoleHolder, tenant)
	}
	if !holderSubject.MatchString(subject) {
		return fmt.Errorf("%w: subject %q is not an account id", ErrInvalidRoleHolder, subject)
	}
	return nil
}

// validHolder says what is wrong with an entry, or nothing. now is the time
// an expiry must lie after.
func validHolder(h RoleHolder, now time.Time) error {
	if err := validHolderKey(h.Role, h.Tenant, h.Subject); err != nil {
		return err
	}
	for field, v := range map[string]string{"username": h.Username, "email": h.Email} {
		if len(v) > 255 || strings.ContainsAny(v, "\n\r\"'\\") || strings.TrimSpace(v) != v {
			return fmt.Errorf("%w: %s %q", ErrInvalidRoleHolder, field, v)
		}
	}
	if len(h.Reason) > 500 || strings.ContainsAny(h.Reason, "\n\r\t\\") || strings.TrimSpace(h.Reason) != h.Reason {
		return fmt.Errorf("%w: the reason is one line of at most 500 characters", ErrInvalidRoleHolder)
	}
	if h.Username == "" {
		return fmt.Errorf("%w: the login is missing", ErrInvalidRoleHolder)
	}
	if h.Reason == "" {
		return fmt.Errorf("%w: the reason is missing", ErrInvalidRoleHolder)
	}
	scope, ok := authz.HolderScope(h.Role, h.Tenant)
	if !ok {
		return fmt.Errorf("%w: %s is held by people of the platform tenant only", ErrInvalidRoleHolder, h.Role)
	}
	if h.Scope != scope {
		return fmt.Errorf("%w: %s held by a person of tenant %s has scope %s, not %q", ErrInvalidRoleHolder, h.Role, h.Tenant, scope, h.Scope)
	}
	if h.ExpiresAt != "" {
		at, err := time.Parse(time.RFC3339, h.ExpiresAt)
		if err != nil {
			return fmt.Errorf("%w: expiresAt %q is not an RFC 3339 time", ErrInvalidRoleHolder, h.ExpiresAt)
		}
		if !at.After(now) {
			return fmt.Errorf("%w: expiresAt %s has passed", ErrInvalidRoleHolder, h.ExpiresAt)
		}
	}
	return nil
}

// SetRoleHolder gives a person a role, or restates it: one commit to the
// claim. The tenant has to be one git declares, and either the platform
// tenant, whose people are the kernel realm's, or one with a realm of its
// own. That the person is an administrator of that tenant, where the role
// asks for one, is the caller's to have checked. An empty scope is filled in
// with the one the role has for the tenant's people.
func (g *GitOps) SetRoleHolder(ctx context.Context, h RoleHolder, meta Meta) (Result, error) {
	if h.Scope == "" {
		h.Scope, _ = authz.HolderScope(h.Role, h.Tenant)
	}
	if err := validHolder(h, time.Now()); err != nil {
		return Result{}, err
	}
	names, err := g.Tenants(ctx)
	if err != nil {
		return Result{}, err
	}
	if !slices.Contains(names, h.Tenant) {
		return Result{}, fmt.Errorf("%w: this cluster has no tenant %q", ErrInvalidRoleHolder, h.Tenant)
	}
	adopts, err := g.IsPlatformTenant(ctx, h.Tenant)
	if err != nil {
		return Result{}, err
	}
	if adopts != (h.Tenant == authz.PlatformTenant) {
		return Result{}, fmt.Errorf("%w: tenant %s has no realm of its own, so nobody of it can be named", ErrInvalidRoleHolder, h.Tenant)
	}
	message := fmt.Sprintf("feat(cluster): %s of tenant %s holds %s over %s (via %s)", h.Username, h.Tenant, h.Role, h.Scope, meta.actor())
	return g.editRoleHolders(ctx, message, meta, func(list []RoleHolder) []RoleHolder {
		list = slices.DeleteFunc(list, func(e RoleHolder) bool {
			return e.Role == h.Role && e.Tenant == h.Tenant && e.Subject == h.Subject
		})
		return append(list, h)
	})
}

// RemoveRoleHolder takes the role away: the entry goes, and with it the
// tuple once the operator has projected the claim.
func (g *GitOps) RemoveRoleHolder(ctx context.Context, role, tenant, subject string, meta Meta) (Result, error) {
	if err := validHolderKey(role, tenant, subject); err != nil {
		return Result{}, err
	}
	message := fmt.Sprintf("feat(cluster): an account of tenant %s no longer holds %s (via %s)", tenant, role, meta.actor())
	return g.editRoleHolders(ctx, message, meta, func(list []RoleHolder) []RoleHolder {
		return slices.DeleteFunc(list, func(e RoleHolder) bool {
			return e.Role == role && e.Tenant == tenant && e.Subject == subject
		})
	})
}

func (g *GitOps) editRoleHolders(ctx context.Context, message string, meta Meta, change func([]RoleHolder) []RoleHolder) (Result, error) {
	return g.applyClaim(ctx, message, meta, func(text string) (string, string, bool, error) {
		have, err := parseRoleHolders(text)
		if err != nil {
			return text, "", false, err
		}
		want := sortedHolders(change(append([]RoleHolder(nil), have...)))
		if slices.Equal(have, want) {
			return text, "unchanged", false, nil
		}
		out, changed, err := setKeyPath(text, "spec", []string{"platformAccess", "roleHolders"}, holdersLeaf(want))
		if errors.Is(err, errNoRoot) {
			return text, "", false, errors.New("the claim has no spec block")
		}
		if err != nil {
			return text, "", false, err
		}
		if !changed {
			return text, "unchanged", false, nil
		}
		back, err := parseRoleHolders(out)
		if err != nil {
			return text, "", false, fmt.Errorf("the edited claim is not valid YAML: %w", err)
		}
		if !slices.Equal(back, want) {
			return text, "", false, errors.New("the edited claim does not read back as written")
		}
		return out, "updated", true, nil
	})
}

// holdersLeaf writes the list as a block of maps under its key, one entry
// per holder, in the claim's own editor (settings.go).
type holdersLeaf []RoleHolder

func (l holdersLeaf) render(indent int, key string) []string {
	pad := strings.Repeat(" ", indent)
	if len(l) == 0 {
		return []string{pad + key + ": []"}
	}
	out := []string{pad + key + ":"}
	for _, h := range l {
		out = append(out, fmt.Sprintf("%s  - role: %s", pad, quoteScalar(h.Role)))
		if h.Scope != "" {
			out = append(out, fmt.Sprintf("%s    scope: %s", pad, quoteScalar(h.Scope)))
		}
		out = append(out,
			fmt.Sprintf("%s    tenant: %s", pad, quoteScalar(h.Tenant)),
			fmt.Sprintf("%s    subject: %s", pad, quoteScalar(h.Subject)),
			fmt.Sprintf("%s    username: %s", pad, quoteScalar(h.Username)))
		for _, f := range [][2]string{{"email", h.Email}, {"reason", h.Reason}, {"expiresAt", h.ExpiresAt}} {
			if f[1] != "" {
				out = append(out, fmt.Sprintf("%s    %s: %s", pad, f[0], quoteScalar(f[1])))
			}
		}
	}
	return out
}

// extent is the key line and every line indented deeper than it: the
// entries, and any comment or blank line among them.
func (holdersLeaf) extent(lines []string, at, indent int) int {
	return listLeaf(nil).extent(lines, at, indent)
}

func (l holdersLeaf) same(lines []string, at, indent int) bool {
	want := l.render(indent, "")
	n := l.extent(lines, at, indent)
	if n != len(want) {
		return false
	}
	for i := 1; i < n; i++ {
		if strings.TrimRight(lines[at+i], " ") != want[i] {
			return false
		}
	}
	return true
}

// --- the tenant's switch ----------------------------------------------------

func parseTenantPlatformAccess(text string) (bool, error) {
	var doc struct {
		Spec struct {
			PlatformAccess *struct {
				AdminsEnter bool `json:"adminsEnter"`
			} `json:"platformAccess"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return false, fmt.Errorf("parse tenant manifest: %w", err)
	}
	return doc.Spec.PlatformAccess != nil && doc.Spec.PlatformAccess.AdminsEnter, nil
}

// TenantPlatformAccess reads whether the platform's administrators may sign
// in to a tenant.
func (g *GitOps) TenantPlatformAccess(ctx context.Context, tenant string) (bool, error) {
	if !ValidName(tenant) {
		return false, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return false, err
	}
	return parseTenantPlatformAccess(string(raw))
}

// SetTenantPlatformAccess writes the switch, or removes it when off.
func (g *GitOps) SetTenantPlatformAccess(ctx context.Context, tenant string, on bool, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	verb := "let the platform's administrators sign in to the tenant"
	if !on {
		verb = "stop the platform's administrators signing in to the tenant"
	}
	msg := fmt.Sprintf("feat(%s): %s (via %s)", tenant, verb, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantPlatformAccess(text)
		if err != nil {
			return text, "", false, err
		}
		if on && adoptsAnotherRealm(text, tenant) {
			return text, "", false, ErrPlatformAccessPlatformTenant
		}
		if have == on {
			return text, "unchanged", false, nil
		}
		var block []string
		if on {
			block = []string{"  platformAccess:", "    adminsEnter: true"}
		}
		out, err := setSpecBlock(text, []string{"platformAccess"}, block)
		if err != nil {
			return text, "", false, err
		}
		back, err := parseTenantPlatformAccess(out)
		if err != nil {
			return text, "", false, fmt.Errorf("the edited manifest is not valid YAML: %w", err)
		}
		if back != on {
			return text, "", false, errors.New("the edited manifest does not read back as written")
		}
		return out, "updated", true, nil
	})
}
