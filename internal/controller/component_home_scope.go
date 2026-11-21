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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A component that calls a home as the person (spec.integrations[].asPerson)
// asks the tenant's realm for the person's token with the home's scope
// (HomeTokenScope), which it cannot work out itself: it is told the home's
// address and never which app that is. Its chart is told the scope under
// package.valueMapping.integrations.<contract>.homeScopeKey, as a value of
// its release, while the tenant's administrator grants the contract.
//
// A value and not an entry of a Secret: the scope is a name, and the
// component's vouching Secret is External Secrets' to write, which puts it
// back to its template on every change and turns the client secret over
// with it.

// +kubebuilder:rbac:groups=gentianos.io,resources=integrationbindings,verbs=get;list;watch

// homeScopeValues are the scopes of the homes granted to the component, at
// the keys its profile names for them.
func homeScopeValues(ctx context.Context, c client.Reader, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	keys := homeScopeKeys(profile)
	if len(keys) == 0 {
		return out, nil
	}
	bindings := &gentianov1alpha1.IntegrationBindingList{}
	if err := c.List(ctx, bindings, client.InNamespace(comp.Namespace)); err != nil {
		if apimeta.IsNoMatchError(err) {
			return out, nil
		}
		return nil, fmt.Errorf("list integration bindings in %s: %w", comp.Namespace, err)
	}
	for i := range bindings.Items {
		ib := &bindings.Items[i]
		key := keys[ib.Spec.Contract]
		if key == "" || ib.Spec.Consumer.App != comp.Name || ib.Spec.Provider.App == "" ||
			ib.DeletionTimestamp != nil || !apimeta.IsStatusConditionTrue(ib.Status.Conditions, conditionBindingGranted) {
			continue
		}
		setPath(out, key, HomeTokenScope(ib.Spec.Provider.App))
	}
	return out, nil
}

// homeScopeKeys are, by contract, the value keys a profile names for the
// scope of a home it calls as the person.
func homeScopeKeys(profile *gentianov1alpha1.ComponentProfile) map[string]string {
	if profile == nil || profile.Spec.Package.ValueMapping == nil {
		return nil
	}
	mapping := profile.Spec.Package.ValueMapping.Integrations
	out := map[string]string{}
	for _, in := range profile.Spec.Integrations {
		if m, ok := mapping[in.Contract]; ok && in.AsPerson && m.HomeScopeKey != "" {
			out[in.Contract] = m.HomeScopeKey
		}
	}
	return out
}

// componentOfBinding re-runs a binding's consumer: whether it is granted, and
// which home answers it, is what its chart is told the scope from.
func componentOfBinding() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		ib, ok := obj.(*gentianov1alpha1.IntegrationBinding)
		if !ok || ib.Spec.Consumer.App == "" {
			return nil
		}
		ns := ib.Spec.Consumer.Namespace
		if ns == "" {
			ns = ib.Namespace
		}
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: ns, Name: ib.Spec.Consumer.App}}}
	})
}

// homeScopeChanged passes a binding's events on when what a home's scope is
// read from changes: created or gone, granted or not, another provider.
func homeScopeChanged() predicate.Predicate {
	return predicate.Funcs{
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(*gentianov1alpha1.IntegrationBinding)
			after, ok2 := e.ObjectNew.(*gentianov1alpha1.IntegrationBinding)
			if !ok1 || !ok2 {
				return false
			}
			return before.Spec.Provider.App != after.Spec.Provider.App ||
				apimeta.IsStatusConditionTrue(before.Status.Conditions, conditionBindingGranted) !=
					apimeta.IsStatusConditionTrue(after.Status.Conditions, conditionBindingGranted) ||
				(before.DeletionTimestamp == nil) != (after.DeletionTimestamp == nil)
		},
	}
}
