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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Vouching (requires.services.vouching): a component signs a statement that
// names a person, and the tenant's realm answers with a token of that person
// for one app. The names below are what the tenant's Composition, the
// registrar and the component have to agree on, and are derived in one place.

// VouchingAlias is the component's name in the tenant's realm, twice over:
// the alias of the identity provider entry that makes it a trusted issuer,
// and the id of the client it asks with. A person is linked to the first.
func VouchingAlias(profile string) string { return "vouch-" + profile }

// VouchingIssuer is what the component's statements name as their issuer.
// It is a name and is never fetched: the keys are read from where the
// profile says they are published.
func VouchingIssuer(tenantNamespace, profile string) string {
	return "https://" + VouchingAlias(profile) + "." + tenantNamespace + ".svc"
}

// VouchingSecretName is the Secret of the tenant's namespace the component
// is told what it has to know in.
func VouchingSecretName(profile string) string { return "vouching-" + profile }

// The entries of that Secret.
const (
	VouchingIssuerKey       = "VOUCHING_ISSUER"
	VouchingClientIDKey     = "VOUCHING_CLIENT_ID"
	VouchingClientSecretKey = "VOUCHING_CLIENT_SECRET" //nolint:gosec // Secret key name, not a credential.
	VouchingTokenURLKey     = "VOUCHING_TOKEN_URL"     //nolint:gosec // Secret key name, not a credential.
)

func wantsVouching(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Vouching != nil
}

// keepVouchingIssuer keeps VOUCHING_ISSUER in the component's Secret equal to
// what its profile says now: present while it signs statements (names
// keys), absent while it only exchanges.
//
// The Composition writes the Secret once, with the issuer where the profile
// named keys then, and never again: the client secret in it must not change.
// A profile that starts or stops publishing keys later would otherwise leave
// the component without the issuer it signs as, or with one the realm no
// longer trusts. Only this one key is touched; the issuer is no secret, and
// the component reads the Secret mounted, so the change reaches it without
// a restart.
//
// A Secret not made yet is left to the Composition, whose template writes
// the issuer as this would.
func (r *TileProjectionReconciler) keepVouchingIssuer(ctx context.Context, namespace, profile string, signs bool) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: VouchingSecretName(profile)}, secret); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	current, present := secret.Data[VouchingIssuerKey]
	want := VouchingIssuer(namespace, profile)
	switch {
	case signs && present && string(current) == want, !signs && !present:
		return nil
	}
	before := secret.DeepCopy()
	if signs {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[VouchingIssuerKey] = []byte(want)
	} else {
		delete(secret.Data, VouchingIssuerKey)
	}
	if err := r.Patch(ctx, secret, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("keep the issuer in %s/%s: %w", namespace, secret.Name, err)
	}
	return nil
}
