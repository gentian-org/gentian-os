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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

// A granted contract opens the way between two apps
// (netpolicy.ContractNetworkPolicies). The network says that a caller is one
// of the provider's granted consumers; it cannot say which. So each granted
// consumer is also given a key of its own for that contract, and the provider
// is given, per contract, the hashes of its consumers' keys with their names.
// A provider that asks for the key knows who is calling, and holds nothing it
// could call as one of them with.
const (
	// contractSecretLabel marks both kinds of Secret, by which is which.
	contractSecretLabel   = "gentianos.io/contract"
	contractSecretKey     = "key"
	contractSecretCallers = "callers"
	// The entries of a consumer's Secret: its key, and whom it is for.
	contractKeyKey      = "CONTRACT_KEY"      //nolint:gosec // Secret key name, not a credential.
	contractProviderKey = "CONTRACT_PROVIDER" //nolint:gosec // Secret key name, not a credential.
)

// contractKeySecretName is the consumer's Secret for one contract.
func contractKeySecretName(consumer, contract string) string {
	return "contract-key-" + consumer + "-" + contract
}

// contractCallersSecretName is the provider's Secret: one entry per contract,
// "<contract>.json", a JSON object of key hash (SHA-256, hex) to consumer.
func contractCallersSecretName(provider string) string { return "contract-callers-" + provider }

// ensureContractKeys keeps those Secrets equal to the contracts that are
// granted now, and removes the ones of a contract that no longer is.
//
// A consumer's key is made once and kept, as its pods read it at start. When
// the grant goes, the consumer's Secret and its hash at the provider go with
// it, in the same reconcile that takes the network path away.
func (r *TenantReconciler) ensureContractKeys(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	bindings []*gentianov1alpha1.IntegrationBinding,
	grants map[string]*gentianov1alpha1.AppGrant,
) error {
	existing := &corev1.SecretList{}
	if err := r.List(ctx, existing, client.InNamespace(nsName), client.HasLabels{contractSecretLabel}); err != nil {
		return fmt.Errorf("list contract secrets in %s: %w", nsName, err)
	}
	have := map[string]*corev1.Secret{}
	for i := range existing.Items {
		s := &existing.Items[i]
		// Only one this reconciler wrote is this reconciler's to keep or remove.
		if s.Labels[managedByLabel] == managedByValue && metav1.IsControlledBy(s, tenant) {
			have[s.Name] = s
		}
	}

	want := map[string]*corev1.Secret{}
	callers := map[string]map[string]map[string]string{} // provider -> contract -> hash -> consumer
	for _, binding := range bindings {
		consumer, provider, contract := binding.Spec.Consumer.App, binding.Spec.Provider.App, binding.Spec.Contract
		if consumer == "" || provider == "" || consumer == provider {
			continue
		}
		if len(netpolicy.EffectiveContractCapabilities(binding, grants[consumer])) == 0 {
			continue
		}
		name := contractKeySecretName(consumer, contract)
		key := ""
		if s := have[name]; s != nil {
			key = string(s.Data[contractKeyKey])
		}
		if key == "" {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			key = base64.RawURLEncoding.EncodeToString(raw)
		}
		want[name] = r.contractSecret(tenant, nsName, name, contractSecretKey, consumer, map[string][]byte{
			contractKeyKey:      []byte(key),
			contractProviderKey: []byte(provider),
		})
		sum := sha256.Sum256([]byte(key))
		if callers[provider] == nil {
			callers[provider] = map[string]map[string]string{}
		}
		if callers[provider][contract] == nil {
			callers[provider][contract] = map[string]string{}
		}
		callers[provider][contract][hex.EncodeToString(sum[:])] = consumer
	}
	for provider, contracts := range callers {
		data := map[string][]byte{}
		for contract, byHash := range contracts {
			// encoding/json writes a map's keys sorted, so the same callers
			// are the same bytes and the Secret is not rewritten for nothing.
			b, err := json.Marshal(byHash)
			if err != nil {
				return err
			}
			data[contract+".json"] = b
		}
		name := contractCallersSecretName(provider)
		want[name] = r.contractSecret(tenant, nsName, name, contractSecretCallers, provider, data)
	}

	for name, desired := range want {
		if err := controllerutil.SetControllerReference(tenant, desired, r.Scheme); err != nil {
			return fmt.Errorf("set owner ref on Secret %s: %w", name, err)
		}
		current := have[name]
		if current == nil {
			if err := r.Create(ctx, desired); err != nil {
				if errors.IsAlreadyExists(err) {
					return fmt.Errorf("secret %s exists and is not this tenant's contract secret", name)
				}
				return fmt.Errorf("create Secret %s: %w", name, err)
			}
			continue
		}
		if equality.Semantic.DeepEqual(current.Data, desired.Data) && equality.Semantic.DeepEqual(current.Labels, desired.Labels) {
			continue
		}
		patch := client.MergeFrom(current.DeepCopy())
		current.Data, current.Labels = desired.Data, desired.Labels
		if err := r.Patch(ctx, current, patch); err != nil {
			return fmt.Errorf("patch Secret %s: %w", name, err)
		}
	}
	for name, stale := range have {
		if _, kept := want[name]; kept {
			continue
		}
		if err := r.Delete(ctx, stale); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Secret %s: %w", name, err)
		}
	}
	return nil
}

func (r *TenantReconciler) contractSecret(tenant *gentianov1alpha1.Tenant, nsName, name, kind, app string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: nsName,
			Labels: map[string]string{
				managedByLabel:      managedByValue,
				tenantLabel:         tenant.Name,
				appLabel:            app,
				contractSecretLabel: kind,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}
