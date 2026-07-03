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

package keycloak

import (
	"fmt"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func tenantPrefix(tenant string) string {
	return "gentian:tenant:" + tenant + ":"
}

// TenantMembersGroup is the Keycloak group for all tenant members.
func TenantMembersGroup(tenant string) string {
	return tenantPrefix(tenant) + "members"
}

// TenantAdminsGroup is the Keycloak group for tenant administrators.
func TenantAdminsGroup(tenant string) string {
	return tenantPrefix(tenant) + "admins"
}

// TenantAppGroup is the Keycloak group for users entitled to an app profile.
func TenantAppGroup(tenant, profile string) string {
	return tenantPrefix(tenant) + "app:" + profile
}

// TenantAppAdminsGroup is the Keycloak group for app administrators.
func TenantAppAdminsGroup(tenant string) string {
	return tenantPrefix(tenant) + "app-admins"
}

// CollectTenantGroupNames returns Gentian entitlement groups for a tenant realm.
// additionalProfiles covers OIDC pack profiles not yet listed on tenant.Spec.Apps.
func CollectTenantGroupNames(tenant *gentianov1alpha1.Tenant, additionalProfiles []string) []string {
	seen := map[string]struct{}{
		TenantMembersGroup(tenant.Name):   {},
		TenantAdminsGroup(tenant.Name):    {},
		TenantAppAdminsGroup(tenant.Name): {},
	}
	names := []string{
		TenantMembersGroup(tenant.Name),
		TenantAdminsGroup(tenant.Name),
		TenantAppAdminsGroup(tenant.Name),
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
		add(TenantAppGroup(tenant.Name, app.Profile))
	}
	for _, profile := range additionalProfiles {
		add(TenantAppGroup(tenant.Name, profile))
	}
	return names
}

// GroupsJobName is the Crossplane identity Job that creates Gentian groups.
func GroupsJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-gentian-groups-%s", tenantName)
}

// ShellWordList formats values for POSIX sh word-splitting in Job env vars.
func ShellWordList(values []string) string {
	return strings.Join(values, " ")
}
