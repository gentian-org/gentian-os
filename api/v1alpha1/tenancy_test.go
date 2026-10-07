/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
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

func named(name string) *Tenant {
	t := &Tenant{}
	t.Name = name
	return t
}

// A tenant's domain is <name>.<kernel>, under either mode, with one
// exception: the one user tenant of a single-tenancy cluster, named user,
// lives on the cluster's own domain. The platform tenant never does.
func TestEffectiveDomainTenancyModes(t *testing.T) {
	t.Parallel()
	const kd = "platform.example.test"
	for _, c := range []struct{ name, mode, want string }{
		{"demo", TenancyModeMulti, "demo." + kd},
		{"demo", TenancyModeSingle, "demo." + kd},
		{"user", TenancyModeMulti, "user." + kd},
		{"user", TenancyModeSingle, kd},
		{"platform", TenancyModeMulti, "platform." + kd},
		{"platform", TenancyModeSingle, "platform." + kd},
	} {
		if got := named(c.name).EffectiveDomain(kd, c.mode); got != c.want {
			t.Errorf("%s under %s: got %q, want %q", c.name, c.mode, got, c.want)
		}
	}
	if !named("user").OnClusterDomain("single") || named("user").OnClusterDomain("multi") ||
		named("platform").OnClusterDomain("single") || named("demo").OnClusterDomain("single") {
		t.Fatal("OnClusterDomain is true for exactly the tenant named user under single")
	}

	tenant := named("user")
	tenant.Status.Domain = "acme.com"
	for _, mode := range []string{TenancyModeMulti, TenancyModeSingle} {
		if got := tenant.EffectiveDomain(kd, mode); got != "acme.com" {
			t.Fatalf("a custom domain overrides the mode (%s): got %q", mode, got)
		}
	}
	if got := named("user").EffectiveDomain("", TenancyModeSingle); got != "" {
		t.Fatalf("no kernel domain: got %q", got)
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
	// The user tenant of a single-tenancy cluster is on the cluster's own
	// domain, where admin@ is the platform administrator's, in the kernel
	// realm. Its administrator is user-admin@.
	if got := named("user").AdminEmailOrDefault("gtn.host", TenancyModeSingle); got != "user-admin@gtn.host" {
		t.Fatalf("single user tenant: got %q", got)
	}
	if got := named("user").TenantAdminUsername("gtn.host", TenancyModeSingle); got != "user-admin@gtn.host" {
		t.Fatalf("single user tenant login: got %q", got)
	}
	if got := named("user").AdminEmailOrDefault("gtn.host", TenancyModeMulti); got != "admin@user.gtn.host" {
		t.Fatalf("a tenant named user under multi: got %q", got)
	}
	if got := named("platform").AdminEmailOrDefault("gtn.host", TenancyModeSingle); got != "admin@platform.gtn.host" {
		t.Fatalf("platform tenant: got %q", got)
	}

	vanity := &Tenant{}
	vanity.Name = "corp"
	vanity.Status.Domain = "acme.com"
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
		tn.Status.Domain = tc.domain
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
