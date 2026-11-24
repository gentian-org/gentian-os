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

// TenantMembersGroup is the name of the members' group a realm made before
// no group carried a right; PlatformGroupPattern still finds such groups in
// an old bundle, and the platform makes none.
func TenantMembersGroup(tenant string) string {
	return tenantPrefix(tenant) + "members"
}

// PartnerRolesClaim is the claim of the scope partner-<name> that names a
// person's role on the tenant in a partner assertion, and nowhere else:
// PartnerRoleTenantAdmin for an administrator of the tenant,
// PartnerRoleClusterAdmin for an administrator of the cluster, as the
// platform's mapper (gentian-partner-roles) asks the rights store; a person
// who is neither has no claim. PartnerRoleAttribute names no attribute the
// platform writes: no group or account carries the role.
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
