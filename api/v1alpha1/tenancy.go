// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package v1alpha1

import "strings"

const (
	// TenancyModeMulti is the default: many tenants on one cluster; app URLs use
	// {sub}.{tenant}.{kernelDomain} unless spec.domain is set.
	TenancyModeMulti = "multi"

	// TenancyModeSingle is for dedicated single-tenant clusters: one Tenant CR
	// named "default", users in ou=default, flat app URLs on {sub}.{kernelDomain}.
	TenancyModeSingle = "single"

	// SingleTenantName is the required Tenant metadata.name in single-tenancy mode.
	SingleTenantName = "default"

	// SingleTenantLDAPOU is the recommended spec.isolation.ldapOU in single mode.
	SingleTenantLDAPOU = "ou=default"
)

// NormalizeTenancyMode returns TenancyModeSingle or TenancyModeMulti.
func NormalizeTenancyMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), TenancyModeSingle) {
		return TenancyModeSingle
	}
	return TenancyModeMulti
}
