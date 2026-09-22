/*
Copyright 2026 Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import "testing"

// The rule this exists for: an alias into a domain the cluster hosts is refused
// rather than written. It would resolve, Postfix would report delivery, and the
// message would sit in a mailbox Dovecot opens for nobody -- indistinguishable
// from a fix, which is the failure mode the aliases exist to end.
func TestResolveRoleAliasTarget(t *testing.T) {
	t.Parallel()

	hosted := map[string]bool{
		"gentian.cloud":        true,
		"finnor.gentian.cloud": true,
	}

	for _, tc := range []struct {
		name       string
		contact    string
		wantTarget string
		wantReject bool
	}{
		{
			name:    "unset is not an error",
			contact: "",
		},
		{
			name:    "whitespace only is unset",
			contact: "   ",
		},
		{
			name:       "an address off this cluster is written",
			contact:    "ops@example.net",
			wantTarget: "ops@example.net",
		},
		{
			name:       "surrounding whitespace is trimmed, not rejected",
			contact:    "  ops@example.net  ",
			wantTarget: "ops@example.net",
		},
		{
			name:       "the kernel domain is refused",
			contact:    "admin@gentian.cloud",
			wantReject: true,
		},
		{
			name:       "a tenant domain is refused",
			contact:    "admin@finnor.gentian.cloud",
			wantReject: true,
		},
		{
			name:       "case does not evade the hosted-domain check",
			contact:    "Admin@Gentian.Cloud",
			wantReject: true,
		},
		{
			name:       "a bare local part is refused",
			contact:    "administrator",
			wantReject: true,
		},
		{
			name:       "a trailing @ is refused",
			contact:    "administrator@",
			wantReject: true,
		},
		{
			// A subdomain of a hosted domain is NOT itself hosted -- Postfix
			// matches virtual_mailbox_domains exactly, so mail for it is not
			// accepted here and an alias to it leaves the cluster like any other.
			name:       "a subdomain the cluster does not host is allowed",
			contact:    "ops@mail.example.net",
			wantTarget: "ops@mail.example.net",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target, reject := resolveRoleAliasTarget(tc.contact, hosted)
			if target != tc.wantTarget {
				t.Errorf("target = %q, want %q", target, tc.wantTarget)
			}
			if got := reject != ""; got != tc.wantReject {
				t.Errorf("rejected = %v (%q), want %v", got, reject, tc.wantReject)
			}
			if reject != "" && target != "" {
				t.Errorf("refused a contact and still returned a target %q", target)
			}
		})
	}
}

// Every role address RFC 5321 and RFC 2142 require, and nothing high-volume.
// dmarc@ is deliberately absent: it is machine-readable XML that would bury the
// two addresses a person has to read.
func TestRoleAliasLocalParts(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"abuse": true, "postmaster": true}
	if len(roleAliasLocalParts) != len(want) {
		t.Fatalf("roleAliasLocalParts = %v, want exactly %v", roleAliasLocalParts, want)
	}
	for _, lp := range roleAliasLocalParts {
		if !want[lp] {
			t.Errorf("unexpected role local part %q", lp)
		}
	}
}
