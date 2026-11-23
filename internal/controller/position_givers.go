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
	"slices"
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
// spec.positionGivers, with what each such tenant lets it give. Never a
// position that administers the tenant or an app (PositionAdministers),
// whatever a tenant written past the schema and the admission says: those
// are listed apart, as administering, for the registrar to refuse by name.
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
			var positions, administering []string
			for _, p := range g.Positions {
				switch {
				case t.PositionAdministers(p):
					if !slices.Contains(administering, p) {
						administering = append(administering, p)
					}
				case !slices.Contains(positions, p):
					positions = append(positions, p)
				}
			}
			sort.Strings(positions)
			sort.Strings(administering)
			ref := gentianov1alpha1.TenantComponentRef{Tenant: g.Tenant, Profile: g.Component}
			granted[ref] = append(granted[ref], componentkeys.PositionGrant{Tenant: t.Name, Positions: positions, Administering: administering})
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

// registrarURLKey is the registrar's address in a component's key Secret,
// written for a component a tenant names in spec.positionGivers: where it
// gives and takes the positions it was granted.
const registrarURLKey = "REGISTRAR_URL"

// namedAsPositionGiver reports whether any tenant names the component of
// profile in tenant as one that gives its positions.
func namedAsPositionGiver(ctx context.Context, c client.Reader, tenant, profile string) (bool, error) {
	tenants := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenants); err != nil {
		return false, err
	}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		if t.DeletionTimestamp != nil {
			continue
		}
		for _, g := range t.Spec.PositionGivers {
			if g.Tenant == tenant && g.Component == profile && len(g.Positions) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// positionGiversChanged lets a tenant's event through when the components it
// names as position givers may have changed.
func positionGiversChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			t, ok := e.Object.(*gentianov1alpha1.Tenant)
			return ok && len(t.Spec.PositionGivers) > 0
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			t, ok := e.Object.(*gentianov1alpha1.Tenant)
			return ok && len(t.Spec.PositionGivers) > 0
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(*gentianov1alpha1.Tenant)
			after, ok2 := e.ObjectNew.(*gentianov1alpha1.Tenant)
			return ok1 && ok2 && !equality.Semantic.DeepEqual(before.Spec.PositionGivers, after.Spec.PositionGivers)
		},
	}
}

// componentsNamedAsPositionGivers wakes the Components a tenant names as
// position givers, in the tenants they run in, so that their key Secret
// gains or loses the registrar's address. An update is mapped for the old
// and the new tenant both, so a component no longer named is woken too.
func componentsNamedAsPositionGivers(c client.Reader) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		changed, ok := obj.(*gentianov1alpha1.Tenant)
		if !ok {
			return nil
		}
		var out []reconcile.Request
		for _, g := range changed.Spec.PositionGivers {
			holder := &gentianov1alpha1.Tenant{}
			if err := c.Get(ctx, types.NamespacedName{Name: g.Tenant}, holder); err != nil {
				continue
			}
			components := &gentianov1alpha1.ComponentList{}
			if err := c.List(ctx, components, client.InNamespace(holder.NamespaceName())); err != nil {
				continue
			}
			for i := range components.Items {
				comp := &components.Items[i]
				if comp.Spec.ProfileRef.Name == g.Component || comp.Name == g.Component {
					out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}})
				}
			}
		}
		return out
	})
}
