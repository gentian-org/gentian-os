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
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// backupRecipientsEnv names the cluster's age recipients, comma-separated.
// Plumbed from the chart's backupRecipients value.
const backupRecipientsEnv = "BACKUP_AGE_RECIPIENTS"

// BackupRecipientsPath is where the cluster's own age recipients live in
// OpenBao, written when the recovery kit is created.
//
// The public half only. The identity goes into the kit and nowhere else: a key
// stored on the cluster it protects is readable by whoever takes that cluster,
// which is the situation backups exist for.
const BackupRecipientsPath = "gentian-os/kernel/backup/recipients"

// backupRecipientsKey is the field at that path.
const backupRecipientsKey = "recipients"

// clusterRecipients returns the age public keys the platform encrypts to.
//
// These are the recipients a scheduled export uses, and the reason it can run
// with nobody present: the matching identity lives in the recovery kit, off the
// cluster, so an export taken at 03:00 is still readable during the incident
// that made it matter.
//
// Git wins. The environment carries backupRecipients from the cluster's values
// in gentian-deployments — reviewed, versioned, and not writable by the cluster
// — and OpenBao is consulted only when git says nothing.
//
// That order matters more than it looks. The operator holds create and update
// on gentian-os/* in OpenBao, so whoever takes the cluster can put their own
// public key there and every later backup is encrypted to them, willingly, by
// the platform. Git is the copy they cannot rewrite from inside. Reading
// OpenBao first — which this did briefly — made the tamper-resistant store the
// fallback and the tamperable one the authority.
//
// OpenBao remains the answer for a cluster nobody has pinned, which is the
// whole point of generating a key with the recovery kit: the default mode has
// to work before anyone has edited anything.
//
// A disagreement is reported and git is used. Refusing to back up would let
// anyone who can write one OpenBao path stop every backup on the cluster, which
// is a worse failure than the one being defended against.
func (r *TenantExportReconciler) clusterRecipients(ctx context.Context) []string {
	pinned := splitRecipients(os.Getenv(backupRecipientsEnv))
	stored := r.storedRecipients(ctx)

	if len(pinned) == 0 {
		return stored
	}
	if len(stored) > 0 && !slices.Equal(pinned, stored) {
		log.FromContext(ctx).Error(nil,
			"the backup recipient in OpenBao does not match the one pinned in this "+
				"cluster's values; using the pinned one. A recipient that changed "+
				"underneath the platform is how bundles come to be encrypted to "+
				"somebody else",
			"pinned", pinned, "stored", stored, "path", BackupRecipientsPath)
	}
	return pinned
}

// storedRecipients reads what the recovery kit wrote, if anything.
func (r *TenantExportReconciler) storedRecipients(ctx context.Context) []string {
	if r.Reconciler == nil || r.Reconciler.Seeder == nil || r.Reconciler.Seeder.KV() == nil {
		return nil
	}
	data, err := r.Reconciler.Seeder.Read(ctx, BackupRecipientsPath)
	if err != nil {
		return nil
	}
	return splitRecipients(data[backupRecipientsKey])
}

func splitRecipients(raw string) []string {
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
		enc.Recipients = r.clusterRecipients(ctx)
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
	return r.discardExportSecretsIn(ctx, export.Namespace, export.Name)
}

// discardExportSecretsIn removes the Secrets a requester staged for one export
// in the tenant's own namespace — the passphrase, and any one-off destination
// keys.
//
// Only the copies beside the capture Jobs used to be removed, so the originals
// accumulated: nine passphrase Secrets sat in tenant-corp against a single
// surviving export, each holding a live passphrase for a bundle that in most
// cases no longer existed. Every one of them is material the platform was asked
// to hold for the length of one backup.
//
// It also made a name unreusable. The console offers a name derived from the
// clock, so a retry within the same minute proposes the same one, found the
// leftover, and failed — which is how this surfaced.
//
// Selected by label rather than by name so a Secret whose name convention
// changes is still collected, and scoped to this export so a concurrent one
// keeps its own.
func (r *TenantExportReconciler) discardExportSecretsIn(
	ctx context.Context,
	namespace, exportName string,
) error {
	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets,
		client.InNamespace(namespace),
		client.MatchingLabels{backup.ExportLabel: exportName},
	); err != nil {
		return fmt.Errorf("list staged Secrets for export %s: %w", exportName, err)
	}
	for i := range secrets.Items {
		if err := r.Delete(ctx, &secrets.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// recordEncryption publishes what was applied, including the blunt statement of
// whether the platform can still read the result.
func recordEncryption(
	export *gentianov1alpha1.TenantExport,
	enc backup.Encryption,
	clusterRecipients []string,
) {
	status := &gentianov1alpha1.ExportEncryptionStatus{
		Mode:             enc.Mode,
		PlatformReadable: enc.PlatformReadable(clusterRecipients),
	}
	if enc.Mode == gentianov1alpha1.ExportEncryptionRecipient {
		status.Recipients = enc.Recipients
	}
	export.Status.Encryption = status
}
