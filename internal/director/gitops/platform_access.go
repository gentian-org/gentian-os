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

	"sigs.k8s.io/yaml"
)

// The kernel realm as hub for the tenants' administrators (iam.md 1.1c), as
// git declares it.
//
// Two things are written here. On the Cluster claim, who holds
// cluster#security_officer: administrators of the cluster's tenants, each by
// the subject of their account in their tenant's realm
// (spec.platformAccess.securityOfficers). The platform tenant's composition
// renders a linked kernel-realm account and its one membership from each
// entry, and removes them with it. On a Tenant's manifest, whether the
// platform's administrators may sign in to it
// (spec.platformAccess.adminsEnter), which the tenant's own administrators
// set; its realm then composes the identity provider "kernel" for them.
//
// Neither touches the authorization graph: the membership reaches it from
// Keycloak's events, as every membership does.

// SecurityOfficer is one entry of spec.platformAccess.securityOfficers.
type SecurityOfficer struct {
	// Tenant is the tenant whose administrator this is.
	Tenant string `json:"tenant"`
	// Subject is the account's id in that tenant's realm, which outlives a
	// changed address: the federated identity is linked by it.
	Subject string `json:"subject"`
	// Username and Email are what the kernel realm shows and what the person
	// signs in as until their first sign-in refreshes them.
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
}

// ErrInvalidSecurityOfficer is an entry that cannot be composed.
var ErrInvalidSecurityOfficer = errors.New("invalid security officer")

// ErrPlatformAccessPlatformTenant is the tenant's switch asked for the
// platform tenant, whose administrators are the platform's already.
var ErrPlatformAccessPlatformTenant = errors.New(
	"this is the platform tenant: its administrators are the platform's own, " +
		"who are in it already. There is nothing to switch on")

// officerSubject is what a subject may look like: Keycloak's user ids are
// UUIDs, and the composition puts the subject in a resource name.
var officerSubject = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type platformAccessClaim struct {
	Spec struct {
		PlatformAccess struct {
			SecurityOfficers []SecurityOfficer `json:"securityOfficers"`
		} `json:"platformAccess"`
	} `json:"spec"`
}

func parseSecurityOfficers(text string) ([]SecurityOfficer, error) {
	var doc platformAccessClaim
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("parse cluster claim: %w", err)
	}
	return sortedOfficers(doc.Spec.PlatformAccess.SecurityOfficers), nil
}

func sortedOfficers(list []SecurityOfficer) []SecurityOfficer {
	out := append([]SecurityOfficer(nil), list...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

// SecurityOfficers reads who the claim names.
func (g *GitOps) SecurityOfficers(ctx context.Context) ([]SecurityOfficer, error) {
	var doc platformAccessClaim
	if err := g.readClusterClaim(ctx, &doc); err != nil {
		return nil, err
	}
	out := sortedOfficers(doc.Spec.PlatformAccess.SecurityOfficers)
	if out == nil {
		out = []SecurityOfficer{}
	}
	return out, nil
}

// validOfficer says what is wrong with an entry, or nothing.
func validOfficer(o SecurityOfficer) error {
	if !ValidName(o.Tenant) {
		return fmt.Errorf("%w: tenant %q is not a name", ErrInvalidSecurityOfficer, o.Tenant)
	}
	if !officerSubject.MatchString(o.Subject) {
		return fmt.Errorf("%w: subject %q is not an account id", ErrInvalidSecurityOfficer, o.Subject)
	}
	for field, v := range map[string]string{"username": o.Username, "email": o.Email} {
		if len(v) > 255 || strings.ContainsAny(v, "\n\r\"'\\") || strings.TrimSpace(v) != v {
			return fmt.Errorf("%w: %s %q", ErrInvalidSecurityOfficer, field, v)
		}
	}
	if o.Username == "" {
		return fmt.Errorf("%w: the login is missing", ErrInvalidSecurityOfficer)
	}
	return nil
}

// SetSecurityOfficer makes a tenant's administrator a security officer, or
// restates one: one commit to the claim. The tenant has to be one git
// declares, with a realm of its own -- the platform tenant's administrators
// are the kernel realm's already and are never brokered.
func (g *GitOps) SetSecurityOfficer(ctx context.Context, o SecurityOfficer, meta Meta) (Result, error) {
	if err := validOfficer(o); err != nil {
		return Result{}, err
	}
	names, err := g.Tenants(ctx)
	if err != nil {
		return Result{}, err
	}
	if !slices.Contains(names, o.Tenant) {
		return Result{}, fmt.Errorf("%w: this cluster has no tenant %q", ErrInvalidSecurityOfficer, o.Tenant)
	}
	platform, err := g.IsPlatformTenant(ctx, o.Tenant)
	if err != nil {
		return Result{}, err
	}
	if platform {
		return Result{}, fmt.Errorf("%w: %s is the platform tenant, whose people are the kernel realm's; "+
			"a security officer is an administrator of another tenant", ErrInvalidSecurityOfficer, o.Tenant)
	}
	message := fmt.Sprintf("feat(cluster): %s of tenant %s is a security officer (via %s)", o.Username, o.Tenant, meta.actor())
	return g.editSecurityOfficers(ctx, message, meta, func(list []SecurityOfficer) []SecurityOfficer {
		list = slices.DeleteFunc(list, func(e SecurityOfficer) bool { return e.Tenant == o.Tenant && e.Subject == o.Subject })
		return append(list, o)
	})
}

// RemoveSecurityOfficer takes the role away: the entry goes, and with it the
// linked account and its membership once the composition has caught up.
func (g *GitOps) RemoveSecurityOfficer(ctx context.Context, tenant, subject string, meta Meta) (Result, error) {
	if !ValidName(tenant) || !officerSubject.MatchString(subject) {
		return Result{}, fmt.Errorf("%w: tenant %q, subject %q", ErrInvalidSecurityOfficer, tenant, subject)
	}
	message := fmt.Sprintf("feat(cluster): an administrator of tenant %s is no longer a security officer (via %s)", tenant, meta.actor())
	return g.editSecurityOfficers(ctx, message, meta, func(list []SecurityOfficer) []SecurityOfficer {
		return slices.DeleteFunc(list, func(e SecurityOfficer) bool { return e.Tenant == tenant && e.Subject == subject })
	})
}

func (g *GitOps) editSecurityOfficers(ctx context.Context, message string, meta Meta, change func([]SecurityOfficer) []SecurityOfficer) (Result, error) {
	return g.applyClaim(ctx, message, meta, func(text string) (string, string, bool, error) {
		have, err := parseSecurityOfficers(text)
		if err != nil {
			return text, "", false, err
		}
		want := sortedOfficers(change(append([]SecurityOfficer(nil), have...)))
		if slices.Equal(have, want) {
			return text, "unchanged", false, nil
		}
		out, changed, err := setKeyPath(text, "spec", []string{"platformAccess", "securityOfficers"}, officersLeaf(want))
		if errors.Is(err, errNoRoot) {
			return text, "", false, errors.New("the claim has no spec block")
		}
		if err != nil {
			return text, "", false, err
		}
		if !changed {
			return text, "unchanged", false, nil
		}
		back, err := parseSecurityOfficers(out)
		if err != nil {
			return text, "", false, fmt.Errorf("the edited claim is not valid YAML: %w", err)
		}
		if !slices.Equal(back, want) {
			return text, "", false, errors.New("the edited claim does not read back as written")
		}
		return out, "updated", true, nil
	})
}

// officersLeaf writes the list as a block of maps under its key, one entry
// per holder, in the claim's own editor (settings.go).
type officersLeaf []SecurityOfficer

func (l officersLeaf) render(indent int, key string) []string {
	pad := strings.Repeat(" ", indent)
	if len(l) == 0 {
		return []string{pad + key + ": []"}
	}
	out := []string{pad + key + ":"}
	for _, o := range l {
		out = append(out,
			fmt.Sprintf("%s  - tenant: %s", pad, quoteScalar(o.Tenant)),
			fmt.Sprintf("%s    subject: %s", pad, quoteScalar(o.Subject)),
			fmt.Sprintf("%s    username: %s", pad, quoteScalar(o.Username)))
		if o.Email != "" {
			out = append(out, fmt.Sprintf("%s    email: %s", pad, quoteScalar(o.Email)))
		}
	}
	return out
}

// extent is the key line and every line indented deeper than it: the
// entries, and any comment or blank line among them.
func (officersLeaf) extent(lines []string, at, indent int) int { return listLeaf(nil).extent(lines, at, indent) }

func (l officersLeaf) same(lines []string, at, indent int) bool {
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
