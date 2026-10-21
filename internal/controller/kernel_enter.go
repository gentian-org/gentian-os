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
	"reflect"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The platform's administrators entering a tenant (iam.md 1.1c).
//
// A Tenant may let the platform's administrators sign in to it
// (spec.platformAccess.adminsEnter). It gets `kernelBroker`, which composes
// the identity provider "kernel" in its realm; the tenant that adopts the
// kernel realm gets it in `kernelEnter`, which composes the client that
// provider signs in with.

// kernelEnterProjection is what the composite of tenant carries of it: on the
// tenant that adopts the kernel realm, every tenant the platform's
// administrators may enter; on any other, whether it is one of them.
//
// Nothing is projected on a cluster that names no kernel realm or domain.
func (r *TenantReconciler) kernelEnterProjection(ctx context.Context, tenant *gentianov1alpha1.Tenant) (enter []interface{}, kernelBroker bool, err error) {
	if r.KernelRealm == "" || r.KernelDomain == "" || tenant.DeletionTimestamp != nil {
		return nil, false, nil
	}
	if !r.adoptsKernelRealm(tenant) {
		return nil, tenant.PlatformAdminsEnter(), nil
	}
	list := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, list); err != nil {
		return nil, false, err
	}
	byName := map[string]*gentianov1alpha1.Tenant{}
	names := make([]string, 0)
	for i := range list.Items {
		t := &list.Items[i]
		// A tenant on its way out, or one that is itself the kernel realm's,
		// is not entered through a client of the kernel realm.
		if t.DeletionTimestamp == nil && !r.adoptsKernelRealm(t) && t.PlatformAdminsEnter() {
			byName[t.Name] = t
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	enter = []interface{}{}
	for _, n := range names {
		enter = append(enter, map[string]interface{}{"tenant": n, "realm": tenantRealmName(byName[n])})
	}
	return enter, false, nil
}

// tenantEnterFactsChanged lets a Tenant event through when what the kernel
// realm composes of that tenant changed: whether the platform's
// administrators may enter it, which realm it is in, and whether it exists.
func tenantEnterFactsChanged() predicate.Predicate {
	of := func(obj client.Object) []interface{} {
		t, ok := obj.(*gentianov1alpha1.Tenant)
		if !ok {
			return nil
		}
		return []interface{}{t.PlatformAdminsEnter(), tenantRealmName(t), t.DeletionTimestamp != nil}
	}
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc:  func(e event.UpdateEvent) bool { return !reflect.DeepEqual(of(e.ObjectOld), of(e.ObjectNew)) },
	}
}

// tenantsAdoptingKernelRealm maps any event to the tenants whose realm is
// the kernel realm: the clients for entering are theirs to recompose.
func tenantsAdoptingKernelRealm(c client.Client, kernelRealm string) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		if kernelRealm == "" {
			return nil
		}
		list := &gentianov1alpha1.TenantList{}
		if err := c.List(ctx, list); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			if tenantRealmName(&list.Items[i]) == kernelRealm {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: list.Items[i].Name}})
			}
		}
		return out
	})
}
