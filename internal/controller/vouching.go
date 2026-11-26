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

// VouchingKeySecretName is the Secret of the tenant's namespace that holds
// the key the platform issued a component that signs statements. The
// operator alone writes it.
func VouchingKeySecretName(profile string) string { return "vouching-key-" + profile }

// vouchingKeyLabel marks a Secret that holds an issued key, and names the
// profile it was issued to: what pruneVouchingKeys finds them by.
const vouchingKeyLabel = "gentianos.io/vouching-key"

// The entries of that Secret.
const (
	VouchingIssuerKey       = "VOUCHING_ISSUER"
	VouchingClientIDKey     = "VOUCHING_CLIENT_ID"
	VouchingClientSecretKey = "VOUCHING_CLIENT_SECRET" //nolint:gosec // Secret key name, not a credential.
	VouchingTokenURLKey     = "VOUCHING_TOKEN_URL"     //nolint:gosec // Secret key name, not a credential.
	// The key the platform issues a component that signs statements
	// (internal/vouchingkey), and the id its statements name it by: the
	// entries of the Secret VouchingKeySecretName names.
	VouchingSigningKeyKey = "VOUCHING_SIGNING_KEY" //nolint:gosec // Secret key name, not a credential.
	VouchingKeyIDKey      = "VOUCHING_KEY_ID"
)

func wantsVouching(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Vouching != nil
}

// keepVouchingIdentity keeps what a signing component signs as and with
// equal to what its profile says now, and returns the public half of its key
// as the realm takes it (a JWKS).
//
// The key is in a Secret of its own, VouchingKeySecretName, which the
// operator alone writes. Not in the Secret the Composition makes: External
// Secrets owns that one and writes it again whenever it holds anything it
// did not put there, so a key added to it is gone a moment later. While the
// profile says the component signs statements (names keys) the key's Secret
// exists, made here as soon as the component does; while it only exchanges,
// there is none.
//
// A key is made once, and again only when it is gone or is not one the
// platform issues. Deleting the Secret is therefore how a key is replaced;
// the realm is given the new public half on the same reconcile. The
// component mounts both Secrets into one directory and reads them when it
// signs, so a change reaches it without a restart.
//
// The issuer follows the profile in the Composition's Secret, as before.
func (r *TileProjectionReconciler) keepVouchingIdentity(ctx context.Context, namespace, tenant, profile string, signs bool) (string, error) {
	if err := r.keepVouchingIssuer(ctx, namespace, profile, signs); err != nil {
		return "", err
	}
	name := types.NamespacedName{Namespace: namespace, Name: VouchingKeySecretName(profile)}
	secret := &corev1.Secret{}
	err := r.Get(ctx, name, secret)
	found := err == nil
	if err != nil && !errors.IsNotFound(err) {
		return "", err
	}
	if !signs {
		if !found {
			return "", nil
		}
		if err := r.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
			return "", fmt.Errorf("remove the signing key %s/%s: %w", namespace, name.Name, err)
		}
		return "", nil
	}
	key, err := vouchingkey.Parse(secret.Data[VouchingSigningKeyKey])
	current := found && err == nil && string(secret.Data[VouchingKeyIDKey]) == key.ID()
	if err != nil {
		issued, genErr := vouchingkey.Generate()
		if genErr != nil {
			return "", fmt.Errorf("make the signing key %s/%s: %w", namespace, name.Name, genErr)
		}
		if key, err = vouchingkey.Parse(issued); err != nil {
			return "", fmt.Errorf("read the signing key just made for %s/%s: %w", namespace, name.Name, err)
		}
		secret.Data = map[string][]byte{VouchingSigningKeyKey: issued}
	}
	if !current {
		secret.Name, secret.Namespace = name.Name, name.Namespace
		secret.Labels = map[string]string{managedByLabel: managedByValue, tenantLabel: tenant, vouchingKeyLabel: profile}
		secret.Data[VouchingKeyIDKey] = []byte(key.ID())
		if found {
			err = r.Update(ctx, secret)
		} else {
			err = r.Create(ctx, secret)
		}
		if err != nil {
			return "", fmt.Errorf("keep the signing key %s/%s: %w", namespace, name.Name, err)
		}
	}
	set, err := key.JWKS()
	if err != nil {
		return "", fmt.Errorf("the public half of the signing key %s/%s: %w", namespace, name.Name, err)
	}
	return string(set), nil
}

// keepVouchingIssuer keeps VOUCHING_ISSUER in the component's Secret equal to
// what its profile says now: present while it signs statements (names
// keys), absent while it only exchanges. A Secret not made yet is left to
// the Composition, whose template writes the issuer as this would.
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

// pruneVouchingKeys removes the issued keys nobody signs with any more: of a
// profile that no longer vouches at all, or of a component that is gone.
// Listed tenant namespace by tenant namespace, as the operator reads Secrets
// everywhere. signers are, per namespace, the profiles that sign statements
// now.
func (r *TileProjectionReconciler) pruneVouchingKeys(ctx context.Context, namespaces []string, signers map[string]map[string]bool) error {
	for _, ns := range namespaces {
		keys := &corev1.SecretList{}
		if err := r.List(ctx, keys, client.InNamespace(ns), client.HasLabels{vouchingKeyLabel}); err != nil {
			return err
		}
		for i := range keys.Items {
			secret := &keys.Items[i]
			if secret.Labels[managedByLabel] != managedByValue || signers[ns][secret.Labels[vouchingKeyLabel]] {
				continue
			}
			if err := r.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("remove the signing key %s/%s: %w", ns, secret.Name, err)
			}
		}
	}
	return nil
}
