/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// stageDestinationCredential copies a one-off destination's keys next to the
// capture Jobs, the way a passphrase is staged.
//
// Only mode: custom needs this. The policy's own destinations are reached
// through a CredentialRequirement, which ESO already materialises into the
// kernel namespace; these keys belong to one export and are used once.
//
// The copy's name is derived from the export, never from the requester's
// reference. A tenant that could name the staged Secret could aim a capture
// Job at any Secret in the kernel namespace, which is every credential the
// platform holds.
func (r *TenantExportReconciler) stageDestinationCredential(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
) error {
	d := export.Spec.Destination
	if d.Resolved() != gentianov1alpha1.ExportDestinationCustom {
		return nil
	}

	source := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      d.CredentialSecretRef,
		Namespace: export.Namespace,
	}, source)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("destination credential Secret %q not found in %s",
				d.CredentialSecretRef, export.Namespace)
		}
		return err
	}

	// Both keys, checked before anything depends on them. Discovering a
	// missing secretKey when the upload runs would mean an app already paused
	// and a bundle half written.
	data := map[string][]byte{}
	for _, key := range []string{backup.DestinationAccessKeyField, backup.DestinationSecretKeyField} {
		value, ok := source.Data[key]
		if !ok || len(value) == 0 {
			return fmt.Errorf("destination credential Secret %q has no non-empty key %q",
				d.CredentialSecretRef, key)
		}
		data[key] = value
	}
	// The capture Jobs read the endpoint from the policy for a policy
	// destination and from a literal for this one, so only the keys travel.

	copied := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.ExportCredentialSecretName(export.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:        tenantNameFromNamespace(export.Namespace),
				managedByLabel:     managedByValue,
				backup.ExportLabel: export.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}

	existing := &corev1.Secret{}
	getErr := r.Get(ctx, types.NamespacedName{
		Name:      copied.Name,
		Namespace: kernelNamespace,
	}, existing)
	switch {
	case apierrors.IsNotFound(getErr):
		return r.Create(ctx, copied)
	case getErr != nil:
		return getErr
	}

	existing.Data = copied.Data
	return r.Update(ctx, existing)
}

// discardDestinationCredential removes the staged copy.
//
// Called on every terminal path, for the same reason the passphrase is: keys
// the requester supplied for one export are not the platform's to keep, and a
// finished export holding live object-storage credentials is a standing
// credential nobody decided to create.
func (r *TenantExportReconciler) discardDestinationCredential(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.ExportCredentialSecretName(export.Name),
			Namespace: kernelNamespace,
		},
	}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
