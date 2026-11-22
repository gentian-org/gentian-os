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
	"errors"
	"fmt"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	dauthz "github.com/gentian-org/gentian-os/internal/director/authz"
)

// UsageStatisticsCounts counts people for the usage statistics report.
//
// With the credential the operator already configures realms with, and no
// other: the report needs two numbers per tenant, and neither is worth a
// second way into Keycloak. Both are asked of the realm and the group the
// identity reconciler made for the tenant, by the names it made them under,
// which is why the counting is here beside it.
type UsageStatisticsCounts struct {
	Client client.Reader
	// Rights is the rights store, for the counts that follow from who holds
	// what: the people of a tenant where accounts are made by sign-up, and
	// the users of an app.
	Rights dauthz.Store

	mu     sync.Mutex
	admin  *authz.KeycloakAdminClient
	issued [3]string
}

// keycloak is the admin client, kept between calls so that a pass over every
// tenant asks for one token and not one per count. It is rebuilt when the
// credential it was built from has changed.
func (c *UsageStatisticsCounts) keycloak(ctx context.Context) (*authz.KeycloakAdminClient, error) {
	kcURL, kcUser, kcPass, err := authz.LoadKeycloakAdmin(ctx, c.Client)
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

// TenantUsers is the number of enabled accounts in the tenant's realm -- or,
// where people sign up to the realm themselves, the number of the tenant's
// people: an account somebody made on the registration page holds no
// position of the tenant, has nothing of the platform's, and is not a user
// of it.
func (c *UsageStatisticsCounts) TenantUsers(ctx context.Context, tenant *gentianov1alpha1.Tenant) (int, error) {
	kc, err := c.keycloak(ctx)
	if err != nil {
		return 0, err
	}
	if tenant.SignUpEnabled() {
		if c.Rights == nil {
			return 0, errNoRightsStore
		}
		people, err := dauthz.PositionHolders(ctx, c.Rights, tenant.Name, dauthz.PositionUser)
		if err != nil {
			return 0, err
		}
		return c.enabled(ctx, kc, tenant, people)
	}
	return kc.CountRealmUsers(ctx, keycloakRealmName(tenant))
}

// AppUsers is the number of enabled accounts that may use the app, or the
// add-on: whoever holds a role on it, as the rights store names them. An
// app nobody was given has nobody.
func (c *UsageStatisticsCounts) AppUsers(ctx context.Context, tenant *gentianov1alpha1.Tenant, profile string) (int, error) {
	kc, err := c.keycloak(ctx)
	if err != nil {
		return 0, err
	}
	if c.Rights == nil {
		return 0, errNoRightsStore
	}
	_, users, err := dauthz.AppHolders(ctx, c.Rights, tenant.Name, profile)
	if err != nil {
		return 0, err
	}
	return c.enabled(ctx, kc, tenant, users)
}

// errNoRightsStore is why a count that follows from who holds what cannot
// be given: nothing is counted rather than counted as nobody.
var errNoRightsStore = errors.New("the rights store is not connected")

// enabled counts the subjects that are enabled accounts of the tenant's
// realm: the store may still name somebody the realm has since disabled.
func (c *UsageStatisticsCounts) enabled(ctx context.Context, kc *authz.KeycloakAdminClient, tenant *gentianov1alpha1.Tenant, subjects []string) (int, error) {
	if len(subjects) == 0 {
		return 0, nil
	}
	accounts, err := kc.ListRealmUsers(ctx, keycloakRealmName(tenant))
	if err != nil {
		return 0, err
	}
	return len(accountsOf(accounts, subjects)), nil
}
