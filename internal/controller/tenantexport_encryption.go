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
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// backupRecipientsEnv names the cluster's age recipients, comma-separated.
// Plumbed from the chart's backupRecipients value.
const backupRecipientsEnv = "BACKUP_AGE_RECIPIENTS"

// clusterRecipients returns the age public keys the platform can decrypt with.
//
// These are the recipients a scheduled export uses, and the reason it can run
// with nobody present: the matching identity lives in the recovery kit, off
// the cluster, so an export taken at 03:00 is still readable during the
// incident that made it matter.
func clusterRecipients() []string {
	raw := os.Getenv(backupRecipientsEnv)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// passphraseSecretName is the copy placed beside the capture Jobs.
func passphraseSecretName(exportName string) string {
	return "tx-" + exportName + "-passphrase"
}

// resolveEncryption works out how this export's bundle is protected, and
// prepares whatever the capture Jobs need to do it.
//
// It refuses rather than falling back. An export that cannot be encrypted is
// an export that must not run: the bundle is every byte the tenant has, it
// outlives this cluster, and it will be copied to places nobody is watching.
func (r *TenantExportReconciler) resolveEncryption(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
) (backup.Encryption, error) {
	spec := export.Spec.Encryption
	mode := spec.Resolved()

	enc := backup.Encryption{Mode: mode}

	switch mode {
	case gentianov1alpha1.ExportEncryptionRecipient:
		enc.Recipients = clusterRecipients()
		if spec != nil && len(spec.Recipients) > 0 {
			// Requester-supplied recipients replace the cluster's rather than
			// adding to them. Appending would silently keep the platform able
			// to read a bundle an admin asked to be readable only by them, and
			// the whole reason to name your own key is that assumption being
			// false.
			enc.Recipients = spec.Recipients
		}

	case gentianov1alpha1.ExportEncryptionPassphrase:
		if spec == nil || spec.PassphraseSecretRef == nil {
			return enc, fmt.Errorf("mode: passphrase requires spec.encryption.passphraseSecretRef")
		}
		if err := r.stagePassphrase(ctx, export, spec); err != nil {
			return enc, err
		}
		enc.PassphraseSecret = passphraseSecretName(export.Name)
		enc.PassphraseKey = spec.PassphraseKey()
	}

	if err := enc.Validate(); err != nil {
		return enc, err
	}
	return enc, nil
}

// stagePassphrase copies the requester's passphrase next to the capture Jobs.
//
// Jobs run in the kernel namespace and a Secret cannot be read across
// namespaces, so a copy is unavoidable. It is owned by the export, so deleting
// the export takes the passphrase with it even if the controller never gets to
// clean up, and it is deleted explicitly the moment the export finishes.
func (r *TenantExportReconciler) stagePassphrase(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	spec *gentianov1alpha1.ExportEncryption,
) error {
	source := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      spec.PassphraseSecretRef.Name,
		Namespace: export.Namespace,
	}, source)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("passphrase Secret %q not found in %s",
				spec.PassphraseSecretRef.Name, export.Namespace)
		}
		return err
	}

	key := spec.PassphraseKey()
	value, ok := source.Data[key]
	if !ok || len(value) == 0 {
		return fmt.Errorf("passphrase Secret %q has no non-empty key %q",
			spec.PassphraseSecretRef.Name, key)
	}

	copied := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      passphraseSecretName(export.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:        tenantNameFromNamespace(export.Namespace),
				managedByLabel:     managedByValue,
				backup.ExportLabel: export.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{key: value},
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

// discardPassphrase removes the staged copy.
//
// Called on every terminal path. The passphrase is the one piece of an export
// the platform is not supposed to retain, so holding it for even a completed
// export would quietly undo the guarantee the mode exists to provide.
func (r *TenantExportReconciler) discardPassphrase(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      passphraseSecretName(export.Name),
			Namespace: kernelNamespace,
		},
	}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// recordEncryption publishes what was applied, including the blunt statement of
// whether the platform can still read the result.
func recordEncryption(export *gentianov1alpha1.TenantExport, enc backup.Encryption) {
	status := &gentianov1alpha1.ExportEncryptionStatus{
		Mode:             enc.Mode,
		PlatformReadable: enc.PlatformReadable(clusterRecipients()),
	}
	if enc.Mode == gentianov1alpha1.ExportEncryptionRecipient {
		status.Recipients = enc.Recipients
	}
	export.Status.Encryption = status
}
