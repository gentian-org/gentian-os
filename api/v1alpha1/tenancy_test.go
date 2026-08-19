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
	if got := tenant.AdminEmailOrDefault("gtn.host", TenancyModeSingle); got != "admin@gtn.host" {
		t.Fatalf("single: got %q", got)
	}

	vanity := &Tenant{}
	vanity.Name = "corp"
	vanity.Spec.Domain = "acme.com"
	if got := vanity.AdminEmailOrDefault("gtn.host", TenancyModeMulti); got != "admin@acme.com" {
		t.Fatalf("vanity domain: got %q", got)
	}

	// No domain anywhere: .invalid can never resolve, which beats inventing one.
	bare := &Tenant{}
	bare.Name = "corp"
	if got := bare.AdminEmailOrDefault("", TenancyModeMulti); got != "admin@corp.invalid" {
		t.Fatalf("no domain: got %q", got)
	}
}

// The login and the address are one string. They were two — admin-<tenant> and
// admin@<domain> — derived in different places, and they disagreed.
func TestTenantAdminUsernameIsTheAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, domain, kernel, mode string
	}{
		{"corp", "", "gtn.host", TenancyModeMulti},
		{"corp", "", "gtn.host", TenancyModeSingle},
		{"corp", "acme.com", "gtn.host", TenancyModeMulti},
		{"corp", "", "", TenancyModeMulti},
	} {
		tn := &Tenant{}
		tn.Name = tc.name
		tn.Spec.Domain = tc.domain
		got := tn.TenantAdminUsername(tc.kernel, tc.mode)
		want := tn.AdminEmailOrDefault(tc.kernel, tc.mode)
		if got != want {
			t.Errorf("username %q != address %q (domain=%q mode=%s)", got, want, tc.domain, tc.mode)
		}
		if !strings.HasPrefix(got, TenantAdminLocalPart+"@") {
			t.Errorf("local part must be %q, got %q", TenantAdminLocalPart, got)
		}
	}
}

// Two tenants never collide, because the domain distinguishes them.
func TestTenantAdminAddressesAreDistinct(t *testing.T) {
	t.Parallel()
	a, b := &Tenant{}, &Tenant{}
	a.Name, b.Name = "corp", "acme"
	if x, y := a.AdminEmailOrDefault("gtn.host", TenancyModeMulti),
		b.AdminEmailOrDefault("gtn.host", TenancyModeMulti); x == y {
		t.Fatalf("two tenants share an address: both %q", x)
	}
}
