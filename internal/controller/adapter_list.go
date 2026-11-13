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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

// The list of a tenant's adapters, told to the components that may call
// them. A caller is the component that checks every tool call made for a
// person (requires.services.vouching); the adapters admit it and nothing
// else (netpolicy/adapter.go). What it cannot know by itself is which
// adapters its tenant has: so each caller is given a ConfigMap,
// adapters-<component>, whose one key is the list as JSON, from an adapter's
// install name to its address inside the tenant and the app it adapts:
//
//	{"odoo-adapter": {"url": "<its Service, port and path>", "app": "odoo"}}
//
// It follows installs: an adapter is on it while it is installed and says
// where it answers (spec.adapter.endpoint). It holds no credential and
// grants nothing: the way to an adapter is the network policy's, and what a
// call may do is the app's own answer to the credential the call carries.
const (
	// adapterListLabel marks the ConfigMaps written here, for the sweep.
	adapterListLabel = "gentianos.io/adapter-list"
	// AdapterListKey is the ConfigMap's one key.
	AdapterListKey = "ADAPTERS"
)

// AdapterListName is the ConfigMap a caller's list is in.
func AdapterListName(component string) string { return "adapters-" + component }

// adapterListEntry is one adapter as a caller is told of it.
type adapterListEntry struct {
	URL string `json:"url"`
	App string `json:"app"`
}

// adapterList is the list for a tenant: its installed adapters that say
// where they answer. JSON with sorted keys, so the same adapters are the
// same bytes.
func adapterList(namespace string, adapters []string, profiles map[string]*gentianov1alpha1.ComponentProfile) (string, error) {
	list := map[string]adapterListEntry{}
	for _, name := range adapters {
		profile := profiles[name]
		if profile == nil || profile.Spec.Adapter == nil || profile.Spec.Adapter.Endpoint == nil {
			continue
		}
		entry := adapterListEntry{URL: contractURL(profile.Spec.Adapter.Endpoint, namespace)}
		if profile.Spec.Adapter.Adapts != nil {
			entry.App = profile.Spec.Adapter.Adapts.Profile
		}
		list[name] = entry
	}
	raw, err := json.Marshal(list)
	return string(raw), err
}

// ensureAdapterLists writes the list for every caller of the tenant and
// removes the list of a component that is a caller no more.
func (r *TenantReconciler) ensureAdapterLists(ctx context.Context, tenant *gentianov1alpha1.Tenant, in netpolicy.BuildInput) error {
	adapters, callers := netpolicy.AdapterRoles(in)
	list, err := adapterList(in.Namespace, adapters, in.Profiles)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, caller := range callers {
		name := AdapterListName(caller)
		wanted[name] = true
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: in.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			if cm.Labels == nil {
				cm.Labels = map[string]string{}
			}
			cm.Labels[adapterListLabel] = "true"
			cm.Data = map[string]string{AdapterListKey: list}
			return controllerutil.SetControllerReference(tenant, cm, r.Scheme)
		}); err != nil {
			return fmt.Errorf("write the adapter list %s: %w", name, err)
		}
	}
	existing := &corev1.ConfigMapList{}
	if err := r.List(ctx, existing, client.InNamespace(in.Namespace), client.MatchingLabels{adapterListLabel: "true"}); err != nil {
		return err
	}
	for i := range existing.Items {
		if wanted[existing.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &existing.Items[i]); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("remove the adapter list %s: %w", existing.Items[i].Name, err)
		}
	}
	return nil
}
