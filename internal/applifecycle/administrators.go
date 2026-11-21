/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"sort"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// Who administers inside an app.
//
// The app administrator's role is one Keycloak group for the whole tenant
// (keycloak.TenantAppAdminsGroup), and it is not in the rights store: its
// members are made administrators inside each app by the app's own means,
// as the app's profile declares. A screen that reads only the rights store
// would show every app as administered by nobody, so what a profile declares
// is said here.

// The ways the group's members come to administer inside an app.
const (
	// AdministersBySync: the app's sync job puts the members in the in-app
	// role the profile names (hooks.provisioning.privilegedRole).
	AdministersBySync = "sync"
	// AdministersByToken: the roles the profile declares for administrators
	// ride on the group and reach the app in a member's token (the
	// keycloak.AdminRolesAttribute group attribute).
	AdministersByToken = "token"
	// AdministersBySignIn: the realm tells the sign-in sidecar that the
	// person is in the group (requires.services.identity.sidecar).
	AdministersBySignIn = "sign-in"
)

// Administrators is how the tenant's app administrators come to administer
// inside one app.
type Administrators struct {
	// Group is the Keycloak group whose members they are.
	Group string `json:"group"`
	// Ways are how the membership reaches the app, of the Administers
	// constants. Sorted.
	Ways []string `json:"ways"`
	// Role is the in-app role the sync job puts them in, as the profile
	// names it; empty without one.
	Role string `json:"role,omitempty"`
	// TokenRoles are the app's roles a member's token carries. Sorted.
	TokenRoles []string `json:"tokenRoles,omitempty"`
}

// administratorsOf reads from a profile how the tenant's app administrators
// administer inside the app, or nil when the profile maps them to nothing.
func administratorsOf(tenant string, profile *gentianov1alpha1.ComponentProfile) *Administrators {
	if profile == nil {
		return nil
	}
	out := Administrators{Group: keycloak.TenantAppAdminsGroup(tenant)}
	if p := profile.Provisioning(); p != nil && p.PrivilegedRole != nil {
		out.Ways = append(out.Ways, AdministersBySync)
		out.Role = p.PrivilegedRole.Name
	}
	// An annotation that cannot be read declares nothing here; the tenant's
	// reconcile is where it is refused.
	if attrs, err := keycloak.ParseGroupAttributes(profile.Annotations[keycloak.GroupAttributesAnnotation]); err == nil {
		for _, role := range attrs[keycloak.AdminRolesAttribute] {
			if role != "" {
				out.TokenRoles = append(out.TokenRoles, role)
			}
		}
		if len(out.TokenRoles) > 0 {
			sort.Strings(out.TokenRoles)
			out.Ways = append(out.Ways, AdministersByToken)
		}
	}
	if s := profile.Services(); s != nil && s.Identity != nil && s.Identity.Sidecar != nil {
		out.Ways = append(out.Ways, AdministersBySignIn)
	}
	if len(out.Ways) == 0 {
		return nil
	}
	sort.Strings(out.Ways)
	return &out
}
