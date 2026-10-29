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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant's country (spec.country) and who it is (its display name,
// languages and spec.organization) are values of a component whose profile
// names valueMapping.platform.countryKey, organizationKey or timeZoneKey. They
// are the Tenant's, not the Component's, so a change to them is carried to
// the release from here.

// countryChanged admits a Tenant update whose country or organization differs.
func countryChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(*gentianov1alpha1.Tenant)
			after, ok2 := e.ObjectNew.(*gentianov1alpha1.Tenant)
			return ok1 && ok2 && (before.Spec.Country != after.Spec.Country ||
				before.Spec.DisplayName != after.Spec.DisplayName ||
				!equality.Semantic.DeepEqual(before.Spec.Locales, after.Spec.Locales) ||
				!equality.Semantic.DeepEqual(before.Spec.Organization, after.Spec.Organization))
		},
	}
}

// componentsToldTheCountry re-runs the components of the tenant whose
// profile names countryKey, organizationKey or timeZoneKey.
func componentsToldTheCountry(c client.Reader) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		tenant, ok := obj.(*gentianov1alpha1.Tenant)
		if !ok {
			return nil
		}
		profiles := &gentianov1alpha1.ComponentProfileList{}
		if err := c.List(ctx, profiles); err != nil {
			return nil
		}
		asking := map[string]struct{}{}
		for i := range profiles.Items {
			if m := platformMapping(&profiles.Items[i]); m != nil && (m.CountryKey != "" || m.OrganizationKey != "" || m.TimeZoneKey != "") {
				asking[profiles.Items[i].Name] = struct{}{}
			}
		}
		if len(asking) == 0 {
			return nil
		}
		components := &gentianov1alpha1.ComponentList{}
		if err := c.List(ctx, components, client.InNamespace(tenant.NamespaceName())); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range components.Items {
			comp := &components.Items[i]
			if _, ok := asking[comp.Spec.ProfileRef.Name]; ok {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}})
			}
		}
		return out
	})
}
