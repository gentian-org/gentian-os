/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// LicenceReportCounts counts people for the licence report.
//
// With the credential the operator already configures realms with, and no
// other: the report needs two numbers per tenant, and neither is worth a
// second way into Keycloak. Both are asked of the realm and the group the
// identity reconciler made for the tenant, by the names it made them under,
// which is why the counting is here beside it.
type LicenceReportCounts struct {
	Client client.Reader

	mu     sync.Mutex
	admin  *authz.KeycloakAdminClient
	issued [3]string
}

// keycloak is the admin client, kept between calls so that a pass over every
// tenant asks for one token and not one per count. It is rebuilt when the
// credential it was built from has changed.
func (c *LicenceReportCounts) keycloak(ctx context.Context) (*authz.KeycloakAdminClient, error) {
	kcURL, kcUser, kcPass, err := loadKeycloakAdmin(ctx, c.Client)
	if err != nil {
		return nil, fmt.Errorf("load keycloak-admin: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := [3]string{kcURL, kcUser, kcPass}; c.admin == nil || c.issued != now {
		c.admin, c.issued = authz.NewKeycloakAdminClient(kcURL, kcUser, kcPass), now
	}
	return c.admin, nil
}

// TenantUsers is the number of enabled accounts in the tenant's realm.
func (c *LicenceReportCounts) TenantUsers(ctx context.Context, tenant *gentianov1alpha1.Tenant) (int, error) {
	kc, err := c.keycloak(ctx)
	if err != nil {
		return 0, err
	}
	return kc.CountRealmUsers(ctx, keycloakRealmName(tenant))
}

// AppUsers is the number of enabled members of the app's entitlement group. A
// group that does not exist yet has nobody in it.
func (c *LicenceReportCounts) AppUsers(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile string) (int, error) {
	kc, err := c.keycloak(ctx)
	if err != nil {
		return 0, err
	}
	members, err := kc.ListGroupMembers(ctx, keycloakRealmName(tenant), keycloak.TenantAppGroup(tenant.Name, profile))
	if err != nil {
		return 0, err
	}
	return len(members), nil
}
