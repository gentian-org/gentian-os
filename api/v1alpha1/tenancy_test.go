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

package v1alpha1

import (
	"strings"
	"testing"
)

func TestNormalizeTenancyMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
	}{
		{"", TenancyModeMulti},
		{"multi", TenancyModeMulti},
		{"MULTI", TenancyModeMulti},
		{"single", TenancyModeSingle},
		{" Single ", TenancyModeSingle},
		{"unknown", TenancyModeMulti},
	} {
		if got := NormalizeTenancyMode(tc.in); got != tc.want {
			t.Fatalf("NormalizeTenancyMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEffectiveDomainTenancyModes(t *testing.T) {
	t.Parallel()
	tenant := &Tenant{}
	tenant.Name = "demo"

	if got := tenant.EffectiveDomain("platform.example.test", TenancyModeMulti); got != "demo.platform.example.test" {
		t.Fatalf("multi: got %q", got)
	}
	if got := tenant.EffectiveDomain("platform.example.test", TenancyModeSingle); got != "platform.example.test" {
		t.Fatalf("single: got %q", got)
	}

	tenant.Spec.Domain = "acme.com"
	if got := tenant.EffectiveDomain("platform.example.test", TenancyModeMulti); got != "acme.com" {
		t.Fatalf("vanity overrides mode: got %q", got)
	}
}

func TestAdminEmailOrDefault(t *testing.T) {
	t.Parallel()
	tenant := &Tenant{}
	tenant.Name = "corp"

	// Derived from the tenant's own domain, so a definition copied between
	// clusters cannot carry the other cluster's domain into the address.
	if got := tenant.AdminEmailOrDefault("gtn.host", TenancyModeMulti); got != "admin@corp.gtn.host" {
		t.Fatalf("multi: got %q", got)
	}

	// A shared mail domain moves the tenant name into the local part, because
	// on a shared domain plain admin@ is not this tenant's to claim — the next
	// tenant provisioned would want the same mailbox.
	shared := &Tenant{}
	shared.Name = "corp"
	shared.Spec.Mail = &TenantMail{Domain: "gtn.host"}
	if got := shared.AdminEmailOrDefault("gtn.host", TenancyModeMulti); got != "admin-corp@gtn.host" {
		t.Fatalf("shared mail domain: got %q", got)
	}
	other := &Tenant{}
	other.Name = "acme"
	other.Spec.Mail = &TenantMail{Domain: "gtn.host"}
	if a, b := shared.AdminEmailOrDefault("gtn.host", TenancyModeMulti),
		other.AdminEmailOrDefault("gtn.host", TenancyModeMulti); a == b {
		t.Fatalf("two tenants on one mail domain must not share an address: both %q", a)
	}

	// mail.domain equal to the tenant's own domain is not "shared", so the name
	// is not added twice.
	own := &Tenant{}
	own.Name = "corp"
	own.Spec.Mail = &TenantMail{Domain: "corp.gtn.host"}
	if got := own.AdminEmailOrDefault("gtn.host", TenancyModeMulti); got != "admin@corp.gtn.host" {
		t.Fatalf("own mail domain: got %q", got)
	}

	// The address must be one the mail stack accepts: same domain the operator
	// writes into virtual_mailbox_domains.
	for _, tn := range []*Tenant{shared, own} {
		addr := tn.AdminEmailOrDefault("gtn.host", TenancyModeMulti)
		md := tn.MailDomainOrDefault("gtn.host", TenancyModeMulti)
		if !strings.HasSuffix(addr, "@"+md) {
			t.Fatalf("address %q is not in the mail domain %q", addr, md)
		}
	}
	if got := tenant.AdminEmailOrDefault("gtn.host", TenancyModeSingle); got != "admin@gtn.host" {
		t.Fatalf("single: got %q", got)
	}

	tenant.Spec.Domain = "acme.com"
	if got := tenant.AdminEmailOrDefault("gtn.host", TenancyModeMulti); got != "admin@acme.com" {
		t.Fatalf("vanity domain: got %q", got)
	}

	// An explicit address is a decision and is never overridden.
	tenant.Spec.AdminEmail = "ops@example.org"
	if got := tenant.AdminEmailOrDefault("gtn.host", TenancyModeMulti); got != "ops@example.org" {
		t.Fatalf("explicit: got %q", got)
	}

	// No domain anywhere: .invalid can never resolve, which beats inventing one.
	bare := &Tenant{}
	bare.Name = "corp"
	if got := bare.AdminEmailOrDefault("", TenancyModeMulti); got != "admin-corp@corp.invalid" {
		t.Fatalf("no domain: got %q", got)
	}
}
