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
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// RoleHandOver is the arbiter's platform path as the hand-over uses it.
type RoleHandOver interface {
	SetRoles(ctx context.Context, tenant, component string, holdings []authz.RoleHolding) error
}

// handsOverRoles is a profile that has the platform keep its people's roles
// in its store (requires.services.authorization.roles: handOver).
func handsOverRoles(p *gentianov1alpha1.ComponentProfile) bool {
	return wantsAuthorization(p) && p.Services().Authorization.HandsOverRoles()
}

// handOverRoles hands every store that is handed roles its people's roles on
// the app, as the rights store answers them now. It runs after the
// projection and the derivation, so a role given or taken -- which patches
// the tenant and so brings the projection back -- reaches the store with
// that pass. A store the arbiter's table does not list yet is handed its
// roles on a later pass.
func handOverRoles(ctx context.Context, c client.Reader, store authz.Store, to RoleHandOver) error {
	keys, err := componentKeys(ctx, c, authorizationKeys, handsOverRoles)
	if err != nil {
		return fmt.Errorf("find the stores handed roles: %w", err)
	}
	byTenant := map[string][]componentKey{}
	for _, k := range keys {
		byTenant[k.Tenant] = append(byTenant[k.Tenant], k)
	}
	tenants := make([]string, 0, len(byTenant))
	for t := range byTenant {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	var errs []error
	for _, tenant := range tenants {
		var profiles []string
		for _, k := range byTenant[tenant] {
			profiles = append(profiles, k.Profile)
		}
		holdings, err := authz.AppRoleHoldings(ctx, store, tenant, profiles)
		if err != nil {
			errs = append(errs, fmt.Errorf("the roles held in tenant %s: %w", tenant, err))
			continue
		}
		for _, k := range byTenant[tenant] {
			err := to.SetRoles(ctx, tenant, k.Component, holdings[k.Profile])
			switch {
			case errors.Is(err, arbiter.ErrNotHanded):
				logf.FromContext(ctx).V(1).Info("the arbiter does not hand roles to the store yet", "tenant", tenant, "component", k.Component)
			case err != nil:
				errs = append(errs, fmt.Errorf("hand the roles to %s/%s: %w", tenant, k.Component, err))
			}
		}
	}
	return errors.Join(errs...)
}
