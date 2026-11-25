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
	"slices"
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The desktops of the tenants whose people sign in to a tenant, which may
// frame that tenant's apps.
//
// A tenant's people who sign in to another tenant (tenant_sign_in.go) open
// its apps from their own desktop: a tile there leads to an app of the other
// tenant, and the desktop opens it in a window, which is a frame. The app's
// frame policy names its own tenant's desktop only, so the browser refuses
// the page there, after the sign-in. The sign-in already takes both tenants
// to declare it -- the target names the source in spec.signInFrom, the source
// names the target in spec.signInTo -- and the same declaration is what lets
// the source's desktop frame the target's apps. Nothing else does: a tenant
// with no sign-in from another is framed by its own desktop alone.
//
// The platform tenant is never among them, whatever it declares: its desktop
// frames no tenant's apps (componentFramers).

// signInDesktops are the desktops of the tenants whose people sign in to
// tenant, by both tenants' declaration, sorted.
func (r *ComponentReconciler) signInDesktops(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	if tenant == nil || len(tenant.Spec.SignInFrom) == 0 {
		return nil, nil
	}
	var hosts []string
	for _, from := range tenant.Spec.SignInFrom {
		if from.Tenant == tenant.Name || from.Tenant == gentianov1alpha1.PlatformTenantName {
			continue
		}
		source := &gentianov1alpha1.Tenant{}
		if err := r.Get(ctx, client.ObjectKey{Name: from.Tenant}, source); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return nil, fmt.Errorf("get tenant %s: %w", from.Tenant, err)
		}
		if source.DeletionTimestamp != nil || !slices.Contains(source.Spec.SignInTo, tenant.Name) {
			continue
		}
		if h := zoneDesktopHost(zoneNamesOf(source, r.KernelDomain, r.TenancyMode, r.KernelRealm)); h != "" {
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return slices.Compact(hosts), nil
}

// signInFramersChanged lets through a tenant whose sign-ins or desktop
// changed: either changes which desktops frame the apps of the tenants it
// pairs with.
func signInFramersChanged(kernelDomain, tenancyMode, kernelRealm string) predicate.Predicate {
	pairs := func(t *gentianov1alpha1.Tenant) bool {
		return len(t.Spec.SignInFrom) > 0 || len(t.Spec.SignInTo) > 0
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			t, ok := e.Object.(*gentianov1alpha1.Tenant)
			return ok && pairs(t)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			t, ok := e.Object.(*gentianov1alpha1.Tenant)
			return ok && pairs(t)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(*gentianov1alpha1.Tenant)
			after, ok2 := e.ObjectNew.(*gentianov1alpha1.Tenant)
			if !ok1 || !ok2 || (!pairs(before) && !pairs(after)) {
				return false
			}
			return !equality.Semantic.DeepEqual(before.Spec.SignInFrom, after.Spec.SignInFrom) ||
				!equality.Semantic.DeepEqual(before.Spec.SignInTo, after.Spec.SignInTo) ||
				(before.DeletionTimestamp == nil) != (after.DeletionTimestamp == nil) ||
				zoneDesktopHost(zoneNamesOf(before, kernelDomain, tenancyMode, kernelRealm)) !=
					zoneDesktopHost(zoneNamesOf(after, kernelDomain, tenancyMode, kernelRealm))
		},
	}
}

// componentsFramedBySignIn re-runs the components of every tenant whose
// framers a change of this one can change: its own, those of the tenants it
// signs in to, and those of the tenants that name it as a source.
func componentsFramedBySignIn(c client.Reader) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		changed, ok := obj.(*gentianov1alpha1.Tenant)
		if !ok {
			return nil
		}
		tenants := &gentianov1alpha1.TenantList{}
		if err := c.List(ctx, tenants); err != nil {
			return nil
		}
		var affected []*gentianov1alpha1.Tenant
		for i := range tenants.Items {
			t := &tenants.Items[i]
			if t.Name == changed.Name ||
				slices.Contains(changed.Spec.SignInTo, t.Name) ||
				slices.ContainsFunc(t.Spec.SignInFrom, func(f gentianov1alpha1.TenantSignInFrom) bool { return f.Tenant == changed.Name }) {
				affected = append(affected, t)
			}
		}
		var out []reconcile.Request
		for _, t := range affected {
			components := &gentianov1alpha1.ComponentList{}
			if err := c.List(ctx, components, client.InNamespace(t.NamespaceName())); err != nil {
				continue
			}
			for i := range components.Items {
				comp := &components.Items[i]
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}})
			}
		}
		return out
	})
}
