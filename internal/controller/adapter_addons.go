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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// toldAddons is the profile's spec.adapter when it asks to be told the
// adapted app's add-ons, or nil.
func toldAddons(profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.AdapterSpec {
	if profile == nil || profile.Spec.Adapter == nil || profile.Spec.Adapter.Adapts == nil ||
		profile.Spec.Adapter.AddonsValuesPath == "" {
		return nil
	}
	return profile.Spec.Adapter
}

// adaptedAddonIDs are, for an adapter that asks for them, the IDs of the
// add-ons selected into the app it adapts in the adapter's own namespace,
// resolved as that app's own render resolves them. Empty when the app is
// not installed there or has none; nil for any other component.
func (r *ComponentReconciler) adaptedAddonIDs(
	ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile,
) ([]string, error) {
	adapter := toldAddons(profile)
	if adapter == nil {
		return nil, nil
	}
	list := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, list, client.InNamespace(comp.Namespace)); err != nil {
		return nil, err
	}
	// Sorted by name, so that two installs of the app (which admission does
	// not make) would still give the same answer every time.
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for i := range list.Items {
		app := &list.Items[i]
		if app.Spec.ProfileRef.Name != adapter.Adapts.Profile || !app.DeletionTimestamp.IsZero() {
			continue
		}
		ids, err := r.addonIDs(ctx, app)
		if err != nil {
			return nil, err
		}
		if ids == nil {
			ids = []string{}
		}
		return ids, nil
	}
	return []string{}, nil
}

// adaptersOfApp re-runs, when a Component changes, the adapters in its
// namespace that are told its add-ons: selecting or removing one changes
// what they are told.
func adaptersOfApp(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		app, ok := obj.(*gentianov1alpha1.Component)
		if !ok {
			return nil
		}
		profiles := &gentianov1alpha1.ComponentProfileList{}
		if err := c.List(ctx, profiles); err != nil {
			return nil
		}
		adapters := map[string]bool{}
		for i := range profiles.Items {
			if a := toldAddons(&profiles.Items[i]); a != nil && a.Adapts.Profile == app.Spec.ProfileRef.Name {
				adapters[profiles.Items[i].Name] = true
			}
		}
		if len(adapters) == 0 {
			return nil
		}
		list := &gentianov1alpha1.ComponentList{}
		if err := c.List(ctx, list, client.InNamespace(app.Namespace)); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			if adapters[list.Items[i].Spec.ProfileRef.Name] {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
					Name: list.Items[i].Name, Namespace: list.Items[i].Namespace,
				}})
			}
		}
		return out
	})
}
