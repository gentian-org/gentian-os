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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A component that vouches for people may take away the links to itself: a
// person who withdraws their consent at the component must not stay linked
// in the realm. It asks the registrar, which holds the realm's credential,
// and shows it the key it was given for the rights check
// (rights-check-<component>). The registrar reads no Secret, so it is told
// which keys exist the way the bouncer is: by their hashes, in a ConfigMap
// it mounts.
const (
	// registrarVouchingKeysConfigMap is the list the registrar mounts
	// (charts/gentian-os/templates/registrar.yaml: the two must agree).
	registrarVouchingKeysConfigMap = "registrar-vouching-keys"
	// registrarVouchingKeysKey is the file in it (internal/registrar,
	// REGISTRAR_VOUCHING_KEYS).
	registrarVouchingKeysKey = "keys.json"
)

// registrarVouchingKey is one entry of the registrar's list
// (internal/registrar.VouchingKey).
type registrarVouchingKey struct {
	Tenant string `json:"tenant"`
	// Component is the profile the component runs: the registrar accepts
	// the key for the links to vouch-<profile> of this tenant's realm, and
	// the realm's entry is named after the profile (VouchingAlias).
	Component string `json:"component"`
	KeyHash   string `json:"keyHash"`
}

// vouchingKeys are the entries the registrar's list must hold: one per
// component that holds a key and whose profile declares vouching now. A
// component that holds a key for the rights check alone is not in it, and
// its key opens nothing at the registrar.
func vouchingKeys(ctx context.Context, c client.Reader) ([]registrarVouchingKey, error) {
	keys, err := componentKeys(ctx, c, wantsVouching)
	if err != nil {
		return nil, err
	}
	out := make([]registrarVouchingKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, registrarVouchingKey{Tenant: k.Tenant, Component: k.Profile, KeyHash: k.KeyHash})
	}
	return out, nil
}

// registrarVouchingKeysDocument is the list as the registrar reads it.
func registrarVouchingKeysDocument(keys []registrarVouchingKey) (string, error) {
	if keys == nil {
		keys = []registrarVouchingKey{}
	}
	b, err := json.Marshal(map[string]interface{}{"keys": keys})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ensureRegistrarVouchingKeys keeps the registrar's list equal to the
// components that vouch for people.
//
// Written where the registrar runs, which is where its realm credentials
// are written too. Written even when it is empty: an entry leaves the list
// by the list being written without it, and a ConfigMap that was left alone
// once nobody vouched would keep the last component's key good.
func (r *GatewayPlatformReconciler) ensureRegistrarVouchingKeys(ctx context.Context) error {
	keys, err := vouchingKeys(ctx, r.Client)
	if err != nil {
		return err
	}
	document, err := registrarVouchingKeysDocument(keys)
	if err != nil {
		return err
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      registrarVouchingKeysConfigMap,
			Namespace: registrarRealmSecretNamespace(),
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string]string{registrarVouchingKeysKey: document},
	}
	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Data[registrarVouchingKeysKey] == document && existing.Labels[managedByLabel] == managedByValue {
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
