// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package v1alpha1

import "testing"

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

	if got := tenant.EffectiveDomain("desk.gentian.org", TenancyModeMulti); got != "demo.desk.gentian.org" {
		t.Fatalf("multi: got %q", got)
	}
	if got := tenant.EffectiveDomain("desk.gentian.org", TenancyModeSingle); got != "desk.gentian.org" {
		t.Fatalf("single: got %q", got)
	}

	tenant.Spec.Domain = "acme.com"
	if got := tenant.EffectiveDomain("desk.gentian.org", TenancyModeMulti); got != "acme.com" {
		t.Fatalf("vanity overrides mode: got %q", got)
	}
}
