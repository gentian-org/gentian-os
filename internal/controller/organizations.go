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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A component that commissions organizations (requires.services.
// organizations) asks the director to add an outside realm to a tenant's
// realm, or to take one away (Tenant spec.brokeredFrom). It presents its own
// key. The director reads no Secret, so it is told which keys exist the way
// the registrar is: by their hashes, in a ConfigMap it mounts, each with the
// tenants whose realm the key may change.
//
// A key is listed for a tenant only when both halves of the grant hold: the
// component's profile declares the requirement, and the tenant names the
// component in spec.organizationCommissioners. Either one alone opens
// nothing.
const (
	// directorCommissionKeysConfigMap is the list the director mounts
	// (charts/gentian-os/templates/director.yaml: the two must agree).
	directorCommissionKeysConfigMap = "director-commission-keys"
	// directorCommissionKeysKey is the file in it (internal/director/api,
	// DIRECTOR_COMMISSION_KEYS).
	directorCommissionKeysKey = "keys.json"
	// componentDirectorURLKey is the director's address in a component's key
	// Secret, for a component that commissions organizations.
	componentDirectorURLKey = "DIRECTOR_URL"
)

// wantsOrganizations reports whether a profile declares that it commissions
// organizations.
func wantsOrganizations(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Organizations != nil
}

// directorCommissionKey is one entry of the director's list
// (internal/componentkeys.Key).
type directorCommissionKey struct {
	Tenant string `json:"tenant"`
	// Component is the profile the component runs.
	Component string `json:"component"`
	KeyHash   string `json:"keyHash"`
	// Tenants are the tenants whose spec.brokeredFrom the key may change.
	Tenants []string `json:"tenants"`
}

// commissionKeys are the entries the director's list must hold.
func commissionKeys(ctx context.Context, c client.Reader) ([]directorCommissionKey, error) {
	keys, err := componentKeys(ctx, c, wantsOrganizations)
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
		for _, ref := range t.Spec.OrganizationCommissioners {
			granted[ref] = append(granted[ref], t.Name)
		}
	}
	var out []directorCommissionKey
	for _, k := range keys {
		targets := granted[gentianov1alpha1.TenantComponentRef{Tenant: k.Tenant, Profile: k.Profile}]
		if len(targets) == 0 {
			continue
		}
		targets = append([]string(nil), targets...)
		sort.Strings(targets)
		out = append(out, directorCommissionKey{
			Tenant: k.Tenant, Component: k.Profile, KeyHash: k.KeyHash, Tenants: targets,
		})
	}
	return out, nil
}

// directorCommissionKeysDocument is the list as the director reads it.
func directorCommissionKeysDocument(keys []directorCommissionKey) (string, error) {
	if keys == nil {
		keys = []directorCommissionKey{}
	}
	b, err := json.Marshal(map[string]interface{}{"keys": keys})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ensureDirectorCommissionKeys keeps the director's list equal to the
// components that may commission organizations, and for which tenants.
//
// Written even when it is empty, for the reason the registrar's list is: an
// entry leaves by the list being written without it.
func (r *GatewayPlatformReconciler) ensureDirectorCommissionKeys(ctx context.Context) error {
	keys, err := commissionKeys(ctx, r.Client)
	if err != nil {
		return err
	}
	document, err := directorCommissionKeysDocument(keys)
	if err != nil {
		return err
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      directorCommissionKeysConfigMap,
			Namespace: registrarRealmSecretNamespace(),
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string]string{directorCommissionKeysKey: document},
	}
	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Data[directorCommissionKeysKey] == document && existing.Labels[managedByLabel] == managedByValue {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = desired.Data
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels[managedByLabel] = managedByValue
	return r.Patch(ctx, existing, patch)
}
