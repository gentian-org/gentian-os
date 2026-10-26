/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"fmt"

	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// InheritAddonGrant gives an addon the access its app has, when nobody has
// the addon yet: the tenant's members when the app is for everyone (as
// GrantAppByDefault, which marks the addon's group too), and otherwise the
// people in the app's own group.
//
// It is the migration of addons installed before an addon had a grant of its
// own: their groups were made and nobody was put in them, so their tiles
// showed to no one. An addon whose group already has somebody in it has a
// grant, and is left as it is. done is false, with no error, while the
// addon's group does not exist.
//
// To be removed with Tenant.Status.AddonGrantsInherited.
func (s *Service) InheritAddonGrant(ctx context.Context, tenantName, app, addon string, everyone bool) (done bool, err error) {
	defer s.lockApp(tenantName, addon)()
	tenant, err := s.getTenant(ctx, tenantName)
	if err != nil {
		return false, err
	}
	realm := keycloak.RealmName(tenant)
	kcURL, kcUser, kcPass, err := s.loadKeycloakAdmin(ctx)
	if err != nil {
		return false, fmt.Errorf("load keycloak admin credentials: %w", err)
	}
	kc := authz.NewKeycloakAdminClient(kcURL, kcUser, kcPass)
	addonGroup := keycloak.TenantAppGroup(tenantName, addon)
	exists, err := kc.GroupExists(ctx, realm, addonGroup)
	if err != nil {
		return false, fmt.Errorf("look up the addon's group: %w", err)
	}
	if !exists {
		return false, nil
	}
	have, err := kc.ListGroupMembers(ctx, realm, addonGroup)
	if err != nil {
		return false, fmt.Errorf("list the addon's group: %w", err)
	}
	if len(have) > 0 {
		return true, nil
	}
	if everyone {
		if err := s.provisionAppGroupUsers(ctx, tenant, addon); err != nil {
			return false, err
		}
		return true, nil
	}
	people, err := kc.ListGroupMembers(ctx, realm, keycloak.TenantAppGroup(tenantName, app))
	if err != nil {
		return false, fmt.Errorf("list the app's group: %w", err)
	}
	if len(people) == 0 {
		return true, nil
	}
	groupID, err := kc.EnsureGroup(ctx, realm, addonGroup, nil)
	if err != nil {
		return false, fmt.Errorf("look up the addon's group: %w", err)
	}
	for _, u := range people {
		if err := kc.AddUserToGroup(ctx, realm, u.ID, groupID); err != nil {
			return false, fmt.Errorf("add user %s to group %s: %w", u.Username, addonGroup, err)
		}
	}
	return true, nil
}
