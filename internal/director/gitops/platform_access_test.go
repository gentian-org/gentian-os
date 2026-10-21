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

	"sigs.k8s.io/yaml"
)

// The holders are written as a block of maps under spec.platformAccess, in
// the claim's own line editor: a commented placeholder is revived, the
// comments around it survive, and what was written reads back.
func TestSecurityOfficersAreWrittenIntoTheClaimAndReadBack(t *testing.T) {
	t.Parallel()
	claim := claimFixture + `  # Who holds the security officers' role.
  # platformAccess:
  #   brokeredSecondFactor: required
  #   securityOfficers: []
`
	want := []SecurityOfficer{
		{Tenant: "acme", Subject: "7d0f3c2e", Username: "it@acme.example", Email: "it@acme.example"},
		{Tenant: "beta", Subject: "0a1b", Username: "ciso@beta.example"},
	}
	out, changed, err := setKeyPath(claim, "spec", []string{"platformAccess", "securityOfficers"}, officersLeaf(sortedOfficers(want)))
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	for _, line := range []string{
		"  platformAccess:",
		"    securityOfficers:",
		"      - tenant: acme",
		"        subject: 7d0f3c2e",
		"        username: \"it@acme.example\"",
		"        email: \"it@acme.example\"",
		"      - tenant: beta",
		"        username: \"ciso@beta.example\"",
		"  #   brokeredSecondFactor: required",
		"# Who administers this cluster, by the Keycloak group they are in.",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing %q in:\n%s", line, out)
		}
	}
	if strings.Contains(out, "email: \"\"") || strings.Contains(out, "securityOfficers: []") {
		t.Fatalf("an empty address is written, or the placeholder stayed:\n%s", out)
	}
	var probe map[string]any
	if err := yaml.Unmarshal([]byte(out), &probe); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, out)
	}
	back, err := parseSecurityOfficers(out)
	if err != nil || !slices.Equal(back, want) {
		t.Fatalf("read back %v (%v), want %v", back, err, want)
	}

	// The same list again is no change; a shorter one replaces the block
	// whole; none leaves an empty list the schema accepts.
	if _, changed, _ := setKeyPath(out, "spec", []string{"platformAccess", "securityOfficers"}, officersLeaf(want)); changed {
		t.Fatal("writing what is there changed the claim")
	}
	fewer, _, err := setKeyPath(out, "spec", []string{"platformAccess", "securityOfficers"}, officersLeaf(want[1:]))
	if err != nil {
		t.Fatal(err)
	}
	if back, _ := parseSecurityOfficers(fewer); !slices.Equal(back, want[1:]) || strings.Contains(fewer, "it@acme.example") {
		t.Fatalf("the first entry is still there:\n%s", fewer)
	}
	none, _, err := setKeyPath(fewer, "spec", []string{"platformAccess", "securityOfficers"}, officersLeaf(nil))
	if err != nil || !strings.Contains(none, "    securityOfficers: []\n") {
		t.Fatalf("err=%v\n%s", err, none)
	}
	if back, _ := parseSecurityOfficers(none); len(back) != 0 {
		t.Fatalf("an empty list reads back as %v", back)
	}
}

func TestASecurityOfficerIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	good := SecurityOfficer{Tenant: "acme", Subject: "7d0f3c2e-5b1a-4e9c-9f1e-2a3b4c5d6e7f", Username: "it@acme.example", Email: "it@acme.example"}
	if err := validOfficer(good); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]SecurityOfficer{
		"tenant not a name":  {Tenant: "Acme", Subject: "1", Username: "x"},
		"subject with space": {Tenant: "acme", Subject: "not a subject", Username: "x"},
		"no login":           {Tenant: "acme", Subject: "1"},
		"login with a quote": {Tenant: "acme", Subject: "1", Username: `it"@acme`},
		"address with a newline": {Tenant: "acme", Subject: "1", Username: "x", Email: "a\nb"},
		"login too long":     {Tenant: "acme", Subject: "1", Username: strings.Repeat("a", 256)},
	} {
		if err := validOfficer(bad); err == nil {
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
