/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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

// TenantPlatformAdminsGroup is the Keycloak group the platform's
// administrators are in while they are in a tenant's realm through the
// identity provider "kernel" (Tenant spec.platformAccess.adminsEnter), put
// there and taken out again at every sign-in like the admins group. It
// grants nothing: no role in the kernel store, nothing on the desktop or in
// an app. It exists so that a partner assertion can say that the person is
// a cluster administrator (PartnerRoleClusterAdmin), and for nothing else.
func TenantPlatformAdminsGroup(tenant string) string {
	return tenantPrefix(tenant) + "platform-admins"
}

// PartnerRoleAttribute is the group attribute that names the role a partner
// assertion carries for the group's members, under the claim
// PartnerRolesClaim of the scope partner-<name> and nowhere else: the
// mapper on that scope aggregates it over the person's groups, and no other
// mapper reads it (groupAttributeNames in the operator leaves it out). The
// admins group carries PartnerRoleTenantAdmin, the platform-admins group
// PartnerRoleClusterAdmin; a person in neither has no claim.
//
// The attribute is the groups' alone: the realm's user profile declares no
// such attribute, and Keycloak admits no unmanaged attribute on an account,
// so the claim can only ever follow from group membership.
const (
	PartnerRoleAttribute    = "gentianPartnerRole"
	PartnerRolesClaim       = "gentian_roles"
	PartnerRoleTenantAdmin  = "tenant-admin"
	PartnerRoleClusterAdmin = "cluster-admin"
)

// TenantAppGroup is the Keycloak group for users entitled to an app profile.
func TenantAppGroup(tenant, profile string) string {
	return tenantPrefix(tenant) + "app:" + profile
}

// DefaultGrantAttribute marks an app entitlement group that the tenant chose to
// provision rather than merely install. It is what makes the App Store's two
// verbs outlive the click: the admin console pre-selects a group carrying it
// when adding a user, so a provisioned app is opt-out for new people and an
// installed one is opt-in.
const DefaultGrantAttribute = "gentianDefaultGrant"

// TenantAppAdminsGroup is the Keycloak group for app administrators.
func TenantAppAdminsGroup(tenant string) string {
	return tenantPrefix(tenant) + "app-admins"
}

// CollectTenantGroupNames returns Gentian entitlement groups for a tenant realm.
// additionalProfiles covers OIDC pack profiles not yet listed on tenant.Spec.Apps.
func CollectTenantGroupNames(tenant *gentianov1alpha1.Tenant, additionalProfiles []string) []string {
	seen := map[string]struct{}{
		TenantMembersGroup(tenant.Name):        {},
		TenantAdminsGroup(tenant.Name):         {},
		TenantPlatformAdminsGroup(tenant.Name): {},
		TenantAppAdminsGroup(tenant.Name):      {},
	}
	names := []string{
		TenantMembersGroup(tenant.Name),
		TenantAdminsGroup(tenant.Name),
		TenantPlatformAdminsGroup(tenant.Name),
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

// RealmName is the Keycloak realm a tenant's people, groups and clients are
// in: spec.isolation.keycloakRealm when the tenant names one, and otherwise
// the tenant's own name. The platform tenant names the kernel realm this way.
//
// The one place this is decided. Everything that talks to the identity
// provider about a tenant asks here; taking the tenant's name for its realm
// is right only for a tenant that did not say otherwise.
func RealmName(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.KeycloakRealm != "" {
		return tenant.Spec.Isolation.KeycloakRealm
	}
	return tenant.Name
}
