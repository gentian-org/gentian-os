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

func gentianTenantAppAdminsGroup(tenant string) string {
	return gentianTenantPrefix(tenant) + "app-admins"
}

func collectGentianTenantGroupNames(tenant *gentianov1alpha1.Tenant, oidcConfigs []oidcAppConfig) []string {
	seen := map[string]struct{}{
		gentianTenantMembersGroup(tenant.Name):   {},
		gentianTenantAdminsGroup(tenant.Name):    {},
		gentianTenantAppAdminsGroup(tenant.Name): {},
	}
	names := []string{
		gentianTenantMembersGroup(tenant.Name),
		gentianTenantAdminsGroup(tenant.Name),
		gentianTenantAppAdminsGroup(tenant.Name),
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

// shellWordList formats values for POSIX sh word-splitting in Job env vars.
// Do not wrap in shell quotes — literal " characters become part of GROUP_NAME.
func shellWordList(values []string) string {
	return strings.Join(values, " ")
}
