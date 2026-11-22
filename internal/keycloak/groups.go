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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func tenantPrefix(tenant string) string {
	return "gentian:tenant:" + tenant + ":"
}

// TenantMembersGroup is the Keycloak group for all tenant members.
func TenantMembersGroup(tenant string) string {
	return tenantPrefix(tenant) + "members"
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

// GroupsJobName is the tenant's identity Job that settles the realm's email
// claim. It keeps the name it had when it also made the tenant's groups.
func GroupsJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-gentian-groups-%s", tenantName)
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
