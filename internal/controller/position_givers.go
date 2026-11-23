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

// A tenant may let a component, of its own or of another tenant, give its
// people some of its positions and take them again (spec.positionGivers).
// The component asks the registrar, which has the operator give and take as
// for a person who manages the tenant's people, and presents the key it
// holds for acting on the platform (rights-check-<component>). The
// registrar reads no Secret, so it is told which keys exist the way it is
// told of the vouching ones: by their hashes, in a ConfigMap it mounts, each
// with the tenants and positions the key may give.
//
// A key is listed only for what a tenant names: the tenant, the component
// by the profile it runs, and the positions. Nothing else opens anything.
const (
	// registrarPositionGiversConfigMap is the list the registrar mounts
	// (charts/gentian-os/templates/registrar.yaml: the two must agree).
	registrarPositionGiversConfigMap = "registrar-position-givers"
	// registrarPositionGiversKey is the file in it (internal/registrar,
	// REGISTRAR_POSITION_GIVERS).
	registrarPositionGiversKey = "keys.json"
)

// positionGivers are the entries the registrar's list must hold: one per
// component that holds a key and that a tenant names in
// spec.positionGivers, with what each such tenant lets it give. Never an
// administrator's position, whatever a tenant written past the schema says.
func positionGivers(ctx context.Context, c client.Reader) ([]componentkeys.PositionGiver, error) {
	keys, err := componentKeys(ctx, c, rightsCheckKeys, holdsComponentKey)
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
	// What each tenant lets each component give, by component.
	granted := map[gentianov1alpha1.TenantComponentRef][]componentkeys.PositionGrant{}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		if t.DeletionTimestamp != nil {
			continue
		}
		for _, g := range t.Spec.PositionGivers {
			positions := notAdministrators(g.Positions)
			if len(positions) == 0 {
				continue
			}
			sort.Strings(positions)
			ref := gentianov1alpha1.TenantComponentRef{Tenant: g.Tenant, Profile: g.Component}
			granted[ref] = append(granted[ref], componentkeys.PositionGrant{Tenant: t.Name, Positions: positions})
		}
	}
	var out []componentkeys.PositionGiver
	for _, k := range keys {
		gives := granted[gentianov1alpha1.TenantComponentRef{Tenant: k.Tenant, Profile: k.Profile}]
		if len(gives) == 0 {
			continue
		}
		gives = append([]componentkeys.PositionGrant(nil), gives...)
		sort.Slice(gives, func(i, j int) bool { return gives[i].Tenant < gives[j].Tenant })
		out = append(out, componentkeys.PositionGiver{
			Key:   componentkeys.Key{Tenant: k.Tenant, Component: k.Profile, KeyHash: k.KeyHash},
			Gives: gives,
		})
	}
	return out, nil
}

// ensureRegistrarPositionGivers keeps the registrar's list equal to the
// components that may give positions, and which, in which tenants.
func (r *GatewayPlatformReconciler) ensureRegistrarPositionGivers(ctx context.Context) error {
	givers, err := positionGivers(ctx, r.Client)
	if err != nil {
		return err
	}
	return ensureKeyList(ctx, r.Client, registrarPositionGiversConfigMap, registrarPositionGiversKey, givers)
}
