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
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/vouchingkey"
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
// It is a name and is never fetched: the realm is given the key the
// statements are signed with (keepVouchingIdentity).
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
	// The key the platform issues a component that signs statements
	// (internal/vouchingkey), and the id its statements name it by.
	VouchingSigningKeyKey = "VOUCHING_SIGNING_KEY" //nolint:gosec // Secret key name, not a credential.
	VouchingKeyIDKey      = "VOUCHING_KEY_ID"
)

func wantsVouching(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Vouching != nil
}

// keepVouchingIdentity keeps what a signing component signs as and with in
// its Secret equal to what its profile says now, and returns the public half
// of its key as the realm takes it (a JWKS).
//
// While the profile says the component signs statements (names keys), the
// Secret holds the issuer, the key the platform issued and the key's id.
// While it only exchanges, it holds none of the three.
//
// The Composition writes the Secret once and never again: the client secret
// in it must not change. So what follows the profile afterwards is kept
// here, and the key is made here: once, and again only when it is gone or is
// not one the platform issues. Taking the two entries away is therefore how
// a key is replaced; the realm is given the new public half on the same
// reconcile. The component reads the Secret mounted, so a change reaches it
// without a restart.
//
// A Secret not made yet is left to the Composition; the answer is then
// empty, and the key follows on a later reconcile.
func (r *TileProjectionReconciler) keepVouchingIdentity(ctx context.Context, namespace, profile string, signs bool) (string, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: VouchingSecretName(profile)}, secret); err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	before := secret.DeepCopy()
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	var jwks string
	if signs {
		secret.Data[VouchingIssuerKey] = []byte(VouchingIssuer(namespace, profile))
		key, err := vouchingkey.Parse(secret.Data[VouchingSigningKeyKey])
		if err != nil {
			issued, genErr := vouchingkey.Generate()
			if genErr != nil {
				return "", fmt.Errorf("make the signing key of %s/%s: %w", namespace, secret.Name, genErr)
			}
			if key, err = vouchingkey.Parse(issued); err != nil {
				return "", fmt.Errorf("read the signing key just made for %s/%s: %w", namespace, secret.Name, err)
			}
			secret.Data[VouchingSigningKeyKey] = issued
		}
		secret.Data[VouchingKeyIDKey] = []byte(key.ID())
		set, err := key.JWKS()
		if err != nil {
			return "", fmt.Errorf("the public half of the signing key of %s/%s: %w", namespace, secret.Name, err)
		}
		jwks = string(set)
	} else {
		delete(secret.Data, VouchingIssuerKey)
		delete(secret.Data, VouchingSigningKeyKey)
		delete(secret.Data, VouchingKeyIDKey)
	}
	if equality.Semantic.DeepEqual(before.Data, secret.Data) {
		return jwks, nil
	}
	if err := r.Patch(ctx, secret, client.MergeFrom(before)); err != nil {
		return "", fmt.Errorf("keep the signing identity in %s/%s: %w", namespace, secret.Name, err)
	}
	return jwks, nil
}
