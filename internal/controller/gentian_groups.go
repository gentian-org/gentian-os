// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"fmt"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func gentianTenantPrefix(tenant string) string {
	return "gentian:tenant:" + tenant + ":"
}

func gentianTenantMembersGroup(tenant string) string {
	return gentianTenantPrefix(tenant) + "members"
}

func gentianTenantAdminsGroup(tenant string) string {
	return gentianTenantPrefix(tenant) + "admins"
}

func gentianTenantAppGroup(tenant, profile string) string {
	return gentianTenantPrefix(tenant) + "app:" + profile
}

func collectGentianTenantGroupNames(tenant *gentianov1alpha1.Tenant, oidcConfigs []oidcAppConfig) []string {
	seen := map[string]struct{}{
		gentianTenantMembersGroup(tenant.Name): {},
		gentianTenantAdminsGroup(tenant.Name):  {},
	}
	names := []string{
		gentianTenantMembersGroup(tenant.Name),
		gentianTenantAdminsGroup(tenant.Name),
	}
	add := func(group string) {
		if group == "" {
			return
		}
		if _, ok := seen[group]; ok {
			return
		}
		seen[group] = struct{}{}
		names = append(names, group)
	}
	for _, app := range tenant.Spec.Apps {
		add(gentianTenantAppGroup(tenant.Name, app.Profile))
	}
	for _, cfg := range oidcConfigs {
		add(gentianTenantAppGroup(tenant.Name, cfg.profileName))
	}
	return names
}

func gentianGroupsJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-gentian-groups-%s", tenantName)
}

func shellQuoteList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return strings.Join(quoted, " ")
}
