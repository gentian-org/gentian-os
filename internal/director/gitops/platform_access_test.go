/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// The holders are written as a block of maps under spec.platformAccess, in
// the claim's own line editor: a commented placeholder is revived, the
// comments around it survive, and what was written reads back.
func TestRoleHoldersAreWrittenIntoTheClaimAndReadBack(t *testing.T) {
	t.Parallel()
	claim := claimFixture + `  # Who holds which cluster role.
  # platformAccess:
  #   roleHolders: []
`
	want := []RoleHolder{
		{Role: "auditor", Tenant: "beta", Subject: "0a1b", Username: "ciso@beta.example"},
		{Role: "securityOfficer", Tenant: "acme", Subject: "7d0f3c2e", Username: "it@acme.example", Email: "it@acme.example"},
	}
	out, changed, err := setKeyPath(claim, "spec", []string{"platformAccess", "roleHolders"}, holdersLeaf(sortedHolders(want)))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	for _, line := range []string{
		"  platformAccess:",
		"    roleHolders:",
		"      - role: auditor",
		"        tenant: beta",
		"      - role: securityOfficer",
		"        tenant: acme",
		"        subject: 7d0f3c2e",
		"        username: \"it@acme.example\"",
		"        email: \"it@acme.example\"",
		"# Who administers this cluster, by the Keycloak group they are in.",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing %q in:\n%s", line, out)
		}
	}
	if strings.Contains(out, "email: \"\"") || strings.Contains(out, "roleHolders: []") {
		t.Fatalf("an empty address is written, or the placeholder stayed:\n%s", out)
	}
	var probe map[string]any
	if err := yaml.Unmarshal([]byte(out), &probe); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, out)
	}
	back, err := parseRoleHolders(out)
	if err != nil || !slices.Equal(back, want) {
		t.Fatalf("read back %v (%v), want %v", back, err, want)
	}

	// The same list again is no change; a shorter one replaces the block
	// whole; none leaves an empty list the schema accepts.
	if _, changed, _ := setKeyPath(out, "spec", []string{"platformAccess", "roleHolders"}, holdersLeaf(want)); changed {
		t.Fatal("writing what is there changed the claim")
	}
	fewer, _, err := setKeyPath(out, "spec", []string{"platformAccess", "roleHolders"}, holdersLeaf(want[:1]))
	if err != nil {
		t.Fatal(err)
	}
	if back, _ := parseRoleHolders(fewer); !slices.Equal(back, want[:1]) || strings.Contains(fewer, "it@acme.example") {
		t.Fatalf("the second entry is still there:\n%s", fewer)
	}
	none, _, err := setKeyPath(fewer, "spec", []string{"platformAccess", "roleHolders"}, holdersLeaf(nil))
	if err != nil || !strings.Contains(none, "    roleHolders: []\n") {
		t.Fatalf("err=%v\n%s", err, none)
	}
	if back, _ := parseRoleHolders(none); len(back) != 0 {
		t.Fatalf("an empty list reads back as %v", back)
	}
}

func TestARoleHolderIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	good := RoleHolder{Role: "securityOfficer", Scope: "cluster", Tenant: "acme", Subject: "7d0f3c2e-5b1a-4e9c-9f1e-2a3b4c5d6e7f",
		Username: "it@acme.example", Email: "it@acme.example", Reason: "Tom's deputy", ExpiresAt: "2026-12-31T00:00:00Z"}
	if err := validHolder(good, now); err != nil {
		t.Fatal(err)
	}
	for name, ok := range map[string]RoleHolder{
		"a perimeter approver of their own tenant": {Role: "perimeterApprover", Scope: "tenant:acme", Tenant: "acme", Subject: "1", Username: "x", Reason: "r"},
		"a perimeter approver of the platform":     {Role: "perimeterApprover", Scope: "cluster", Tenant: "platform", Subject: "1", Username: "x", Reason: "r"},
		"a service administrator of the platform":  {Role: "serviceAdmin", Scope: "cluster", Tenant: "platform", Subject: "1", Username: "x", Reason: "r"},
		"an auditor of one tenant":                 {Role: "tenantAuditor", Scope: "tenant:acme", Tenant: "platform", Subject: "1", Username: "x", Reason: "r"},
	} {
		if err := validHolder(ok, now); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	reason := func(h RoleHolder) RoleHolder {
		if h.Reason == "" {
			h.Reason = "r"
		}
		if h.Scope == "" {
			h.Scope = "cluster"
		}
		return h
	}
	for name, bad := range map[string]RoleHolder{
		"no reason":                     {Role: "auditor", Scope: "cluster", Tenant: "acme", Subject: "1", Username: "x", Reason: ""},
		"expired":                       reason(RoleHolder{Role: "auditor", Tenant: "acme", Subject: "1", Username: "x", ExpiresAt: "2026-10-22T11:00:00Z"}),
		"an expiry that is no time":     reason(RoleHolder{Role: "auditor", Tenant: "acme", Subject: "1", Username: "x", ExpiresAt: "tomorrow"}),
		"service admin of a tenant":     reason(RoleHolder{Role: "serviceAdmin", Tenant: "acme", Subject: "1", Username: "x"}),
		"shared apps admin of a tenant": reason(RoleHolder{Role: "sharedAppsAdmin", Tenant: "acme", Subject: "1", Username: "x"}),
		"a tenant's approver for all":   reason(RoleHolder{Role: "perimeterApprover", Tenant: "acme", Subject: "1", Username: "x"}),
		"another tenant's approver":     reason(RoleHolder{Role: "perimeterApprover", Scope: "tenant:globex", Tenant: "acme", Subject: "1", Username: "x"}),
		"a tenant auditor of no tenant": reason(RoleHolder{Role: "tenantAuditor", Tenant: "platform", Subject: "1", Username: "x"}),
		"a tenant auditor of platform":  reason(RoleHolder{Role: "tenantAuditor", Scope: "tenant:platform", Tenant: "platform", Subject: "1", Username: "x"}),
		"a tenant auditor of a tenant":  reason(RoleHolder{Role: "tenantAuditor", Scope: "tenant:acme", Tenant: "acme", Subject: "1", Username: "x"}),
		"a reason over lines":           reason(RoleHolder{Role: "auditor", Tenant: "acme", Subject: "1", Username: "x", Reason: "a\nb"}),
		"admin is no holder's":          reason(RoleHolder{Role: "admin", Tenant: "acme", Subject: "1", Username: "x"}),
		"break glass neither":           reason(RoleHolder{Role: "breakGlass", Tenant: "acme", Subject: "1", Username: "x"}),
		"the graph's spelling":          reason(RoleHolder{Role: "security_officer", Tenant: "acme", Subject: "1", Username: "x"}),
		"tenant not a name":             reason(RoleHolder{Role: "auditor", Tenant: "Acme", Subject: "1", Username: "x"}),
		"subject with space":            reason(RoleHolder{Role: "auditor", Tenant: "acme", Subject: "not a subject", Username: "x"}),
		"no login":                      reason(RoleHolder{Role: "auditor", Tenant: "acme", Subject: "1"}),
		"login with a quote":            reason(RoleHolder{Role: "auditor", Tenant: "acme", Subject: "1", Username: `it"@acme`}),
		"address with a newline":        reason(RoleHolder{Role: "auditor", Tenant: "acme", Subject: "1", Username: "x", Email: "a\nb"}),
		"login too long":                reason(RoleHolder{Role: "auditor", Tenant: "acme", Subject: "1", Username: strings.Repeat("a", 256)}),
	} {
		if err := validHolder(bad, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTheTenantsPlatformAccessSwitchIsWrittenAndRemoved(t *testing.T) {
	t.Parallel()
	manifest := "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: acme\nspec:\n  displayName: Acme\n  apps: []\n"
	on, err := parseTenantPlatformAccess(manifest)
	if err != nil || on {
		t.Fatalf("a plain manifest: on=%v err=%v", on, err)
	}
	out, err := setSpecBlock(manifest, []string{"platformAccess"}, []string{"  platformAccess:", "    adminsEnter: true"})
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := parseTenantPlatformAccess(out); !on {
		t.Fatalf("not on:\n%s", out)
	}
	off, err := setSpecBlock(out, []string{"platformAccess"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := parseTenantPlatformAccess(off); on || strings.Contains(off, "platformAccess") {
		t.Fatalf("still on:\n%s", off)
	}
}
