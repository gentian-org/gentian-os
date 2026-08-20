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
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// Volume Jobs run in the tenant namespace — a PVC is only mountable from its
// own namespace — where neither the MinIO credentials nor a passphrase Secret
// exist, and where the originals must not live. So the controller stages a
// copy for exactly as long as the export or restore runs, and removes it on
// every exit path. The staged Secret is the tenant-namespace lifetime of the
// operation, nothing more.

// volumeUploadSecretName names the export's staged copy.
func volumeUploadSecretName(exportName string) string {
	return exportJobName(exportName, "vol", "creds")
}

// restoreVolumeSecretName names the restore's staged copy. A distinct unit
// suffix, because an export and a restore may share a name and a namespace.
func restoreVolumeSecretName(restoreName string) string {
	return exportJobName(restoreName, "vol", "rcreds")
}

// stageVolumeSecret writes the staged copy: the MinIO credentials, plus any
// extra keys (a passphrase for an encrypting export). Idempotent.
func stageVolumeSecret(
	ctx context.Context,
	c client.Client,
	name, namespace, tenantName, owner string,
	extra map[string][]byte,
) error {
	admin := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: backup.MinIOAdminSecret, Namespace: kernelNamespace}, admin); err != nil {
		return fmt.Errorf("read %s: %w", backup.MinIOAdminSecret, err)
	}
	data := map[string][]byte{}
	for _, k := range []string{"endpoint", "accessKey", "secretKey"} {
		v, ok := admin.Data[k]
		if !ok || len(v) == 0 {
			return fmt.Errorf("%s has no non-empty key %q", backup.MinIOAdminSecret, k)
		}
		data[k] = v
	}
	for k, v := range extra {
		data[k] = v
	}

	staged := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				tenantLabel:        tenantName,
				managedByLabel:     managedByValue,
				backup.ExportLabel: owner,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := c.Create(ctx, staged); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return c.Update(ctx, staged)
		}
		return err
	}
	return nil
}

func discardStagedSecret(ctx context.Context, c client.Client, name, namespace string) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := c.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ensureVolumeUploadSecret stages the export's copy into the tenant namespace.
func (r *TenantExportReconciler) ensureVolumeUploadSecret(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenantName string,
	enc backup.Encryption,
) error {
	extra := map[string][]byte{}
	if enc.Mode == gentianov1alpha1.ExportEncryptionPassphrase {
		pp := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: enc.PassphraseSecret, Namespace: kernelNamespace}, pp); err != nil {
			return fmt.Errorf("read passphrase for volume capture: %w", err)
		}
		value, ok := pp.Data[enc.PassphraseKey]
		if !ok || len(value) == 0 {
			return fmt.Errorf("passphrase Secret %q has no non-empty key %q", enc.PassphraseSecret, enc.PassphraseKey)
		}
		extra[enc.PassphraseKey] = value
	}
	return stageVolumeSecret(ctx, r.Client,
		volumeUploadSecretName(export.Name), export.Namespace,
		tenantNameFromNamespace(export.Namespace), export.Name, extra)
}

func (r *TenantExportReconciler) discardVolumeUploadSecret(ctx context.Context, export *gentianov1alpha1.TenantExport) error {
	return discardStagedSecret(ctx, r.Client, volumeUploadSecretName(export.Name), export.Namespace)
}

// ensureRestoreVolumeSecret stages the restore's copy into the tenant
// namespace. Only the MinIO credentials: the decryption key material already
// lives in the tenant namespace, where the operator was told to find it.
func (r *TenantRestoreReconciler) ensureRestoreVolumeSecret(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
) error {
	return stageVolumeSecret(ctx, r.Client,
		restoreVolumeSecretName(restore.Name), restore.Namespace,
		tenantNameFromNamespace(restore.Namespace), restore.Name, nil)
}

func (r *TenantRestoreReconciler) discardRestoreVolumeSecret(ctx context.Context, restore *gentianov1alpha1.TenantRestore) error {
	return discardStagedSecret(ctx, r.Client, restoreVolumeSecretName(restore.Name), restore.Namespace)
}
