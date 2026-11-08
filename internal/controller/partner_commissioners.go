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
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/componentkeys"
)

// A component that commissions partners (requires.services.partners.
// commission) asks the director to add a receiving partner to a tenant's
// realm, or to take one away (Tenant spec.partners). It presents its own key.
// The director reads no Secret, so it is told which keys exist the way the
// registrar is: by their hashes, in a ConfigMap it mounts, each with the
// tenants whose partners the key may change.
//
// A key is listed for a tenant only when both halves of the grant hold: the
// component's profile declares the requirement, and the tenant names the
// component in spec.partnerCommissioners. Either one alone opens nothing.
const (
	// directorCommissionKeysConfigMap is the list the director mounts
	// (charts/gentian-os/templates/director.yaml: the two must agree).
	directorCommissionKeysConfigMap = "director-commission-keys"
	// directorCommissionKeysKey is the file in it (internal/director/api,
	// DIRECTOR_COMMISSION_KEYS).
	directorCommissionKeysKey = "keys.json"
	// componentDirectorURLKey is the director's address in a component's key
	// Secret, for a component that commissions partners.
	componentDirectorURLKey = "DIRECTOR_URL"
)

// commissionKeys are the entries the director's list must hold.
func commissionKeys(ctx context.Context, c client.Reader) ([]componentkeys.Commission, error) {
	keys, err := componentKeys(ctx, c, rightsCheckKeys, wantsPartnerCommission)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenants); err != nil {
		return nil, err
	}
	// Who each tenant names, by component.
	granted := map[gentianov1alpha1.TenantComponentRef][]string{}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		if t.DeletionTimestamp != nil {
			continue
		}
		for _, ref := range t.Spec.PartnerCommissioners {
			granted[ref] = append(granted[ref], t.Name)
		}
	}
	var out []componentkeys.Commission
	for _, k := range keys {
		targets := granted[gentianov1alpha1.TenantComponentRef{Tenant: k.Tenant, Profile: k.Profile}]
		if len(targets) == 0 {
			continue
		}
		targets = append([]string(nil), targets...)
		sort.Strings(targets)
		out = append(out, componentkeys.Commission{
			Key:     componentkeys.Key{Tenant: k.Tenant, Component: k.Profile, KeyHash: k.KeyHash},
			Tenants: targets,
		})
	}
	return out, nil
}

// ensureDirectorCommissionKeys keeps the director's list equal to the
// components that may commission partners, and for which tenants.
func (r *GatewayPlatformReconciler) ensureDirectorCommissionKeys(ctx context.Context) error {
	keys, err := commissionKeys(ctx, r.Client)
	if err != nil {
		return err
	}
	return ensureKeyList(ctx, r.Client, directorCommissionKeysConfigMap, directorCommissionKeysKey, keys)
}
