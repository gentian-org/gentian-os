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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/componentkeys"
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

// vouchingKeys are the entries the registrar's list must hold: one per
// component that holds a key and whose profile declares vouching now. A
// component that holds a key for the rights check alone is not in it, and
// its key opens nothing at the registrar. Component is the profile the
// component runs: the registrar accepts the key for the links to
// vouch-<profile> of this tenant's realm, and the realm's entry is named
// after the profile (VouchingAlias).
func vouchingKeys(ctx context.Context, c client.Reader) ([]componentkeys.Key, error) {
	keys, err := componentKeys(ctx, c, rightsCheckKeys, wantsVouching)
	if err != nil {
		return nil, err
	}
	out := make([]componentkeys.Key, 0, len(keys))
	for _, k := range keys {
		out = append(out, componentkeys.Key{Tenant: k.Tenant, Component: k.Profile, KeyHash: k.KeyHash})
	}
	return out, nil
}

// ensureRegistrarVouchingKeys keeps the registrar's list equal to the
// components that vouch for people.
func (r *GatewayPlatformReconciler) ensureRegistrarVouchingKeys(ctx context.Context) error {
	keys, err := vouchingKeys(ctx, r.Client)
	if err != nil {
		return err
	}
	return ensureKeyList(ctx, r.Client, registrarVouchingKeysConfigMap, registrarVouchingKeysKey, keys)
}

// ensureKeyList keeps a list of component keys (componentkeys.Document)
// equal to entries, in the ConfigMap name under key.
//
// Written where the registrar and the director run, which is where the
// registrar's realm credentials are written too. Written even when it is
// empty: an entry leaves the list by the list being written without it, and
// a ConfigMap that was left alone once nobody held a key would keep the last
// component's key good.
func ensureKeyList[E componentkeys.Entry](ctx context.Context, c client.Client, name, key string, entries []E) error {
	document, err := componentkeys.Document(entries)
	if err != nil {
		return err
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: registrarRealmSecretNamespace(),
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Data: map[string]string{key: document},
	}
	return ensureObject(ctx, c, desired, func(existing *corev1.ConfigMap) bool {
		if existing.Data[key] == document && existing.Labels[managedByLabel] == managedByValue {
			return false
		}
		existing.Data = desired.Data
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[managedByLabel] = managedByValue
		return true
	})
}
