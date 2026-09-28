/*
Copyright 2026 Gentian Organization.

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

import (
	"strings"
	"testing"
)

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

// A recipient map is only safe if it is complete. These pin the two ways it
// could quietly lose mail: dropping an owner, and dropping an address the specs
// require every domain to accept.
func TestDomainRecipients(t *testing.T) {
	t.Parallel()

	t.Run("owners and role addresses are both written", func(t *testing.T) {
		t.Parallel()
		lines, owners := domainRecipients("finnor.example", []string{
			"sibylle@finnor.example",
			"admin@finnor.example",
		})
		if owners != 2 {
			t.Errorf("owners = %d, want 2", owners)
		}
		for _, want := range []string{
			"sibylle@finnor.example finnor.example/",
			"admin@finnor.example finnor.example/",
			"abuse@finnor.example finnor.example/",
			"postmaster@finnor.example finnor.example/",
			"dmarc@finnor.example finnor.example/",
		} {
			if !strings.Contains(lines, want) {
				t.Errorf("missing %q in:\n%s", want, lines)
			}
		}
	})

	t.Run("a user in another domain is not written into this one", func(t *testing.T) {
		t.Parallel()
		lines, owners := domainRecipients("finnor.example", []string{
			"sibylle@finnor.example",
			"someone@corp.example",
		})
		if owners != 1 {
			t.Errorf("owners = %d, want 1", owners)
		}
		if strings.Contains(lines, "corp.example") {
			t.Errorf("leaked a foreign domain into the map:\n%s", lines)
		}
	})

	t.Run("an owner who is already a role address is not written twice", func(t *testing.T) {
		t.Parallel()
		lines, _ := domainRecipients("finnor.example", []string{"postmaster@finnor.example"})
		if got := strings.Count(lines, "postmaster@finnor.example"); got != 1 {
			t.Errorf("postmaster written %d times:\n%s", got, lines)
		}
	})

	t.Run("case is normalised so one address is one line", func(t *testing.T) {
		t.Parallel()
		lines, _ := domainRecipients("finnor.example", []string{
			"Sibylle@Finnor.Example",
			"sibylle@finnor.example",
		})
		if got := strings.Count(lines, "sibylle@finnor.example"); got != 1 {
			t.Errorf("same address written %d times:\n%s", got, lines)
		}
	})

	t.Run("no owners still accepts what the specs require", func(t *testing.T) {
		t.Parallel()
		lines, owners := domainRecipients("finnor.example", nil)
		if owners != 0 {
			t.Errorf("owners = %d, want 0", owners)
		}
		// The caller keeps the catch-all at owners == 0, but the lines must
		// still be well formed -- postmaster may never be refused.
		for _, want := range []string{"abuse@", "dmarc@", "postmaster@"} {
			if !strings.Contains(lines, want) {
				t.Errorf("missing %q in:\n%s", want, lines)
			}
		}
	})
}

// The kernel domain's owners live in the kernel realm, every other key is a
// tenant whose realm is its name. Getting this wrong narrows a domain against
// the wrong realm, which is how a recipient map bounces everyone.
func TestMailRealmForRegistryKey(t *testing.T) {
	t.Parallel()
	r := &TenantReconciler{KernelRealm: "kernel"}
	if got := r.mailRealmForRegistryKey("_kernel"); got != "kernel" {
		t.Errorf("_kernel -> %q, want kernel", got)
	}
	if got := r.mailRealmForRegistryKey("finnor"); got != "finnor" {
		t.Errorf("finnor -> %q, want finnor", got)
	}
}

// The kernel realm's administrator is "administrator" with the address in the
// email field, while a tenant realm's users are created with the address AS the
// username. Reading the login for both is what left the kernel domain reporting
// no owners and kept 352 junk maildirs on the catch-all.
func TestKeycloakRealmUserMailAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		u    keycloakRealmUser
		want string
	}{
		{
			name: "tenant realm: the username is the address",
			u:    keycloakRealmUser{Username: "sibylle@finnor.example", Email: "sibylle@finnor.example", Enabled: true},
			want: "sibylle@finnor.example",
		},
		{
			name: "kernel realm: the login has no domain, the email does",
			u:    keycloakRealmUser{Username: "administrator", Email: "administrator@gentian.cloud", Enabled: true},
			want: "administrator@gentian.cloud",
		},
		{
			name: "the username wins when both are addresses",
			u:    keycloakRealmUser{Username: "a@x.example", Email: "b@y.example", Enabled: true},
			want: "a@x.example",
		},
		{
			name: "a service account with neither is not a recipient",
			u:    keycloakRealmUser{Username: "service-account-odoo-cb", Enabled: true},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.u.mailAddress(); got != tc.want {
				t.Errorf("mailAddress() = %q, want %q", got, tc.want)
			}
		})
	}
}
