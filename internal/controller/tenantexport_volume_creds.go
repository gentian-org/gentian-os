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

// stageVolumeSecret writes the staged copy: the credentials the upload will
// authenticate with, plus any extra keys (a passphrase for an encrypting
// export). Idempotent.
//
// sourceSecret names where those credentials come from, in the kernel
// namespace. For a bundle going to the platform's own storage that is the MinIO
// admin Secret; for an external destination it is the Secret ESO materialised
// from the destination's credential, and the two are different accounts on
// different systems.
//
// Getting that wrong is invisible until the upload runs. Volume Jobs are the
// only ones that stage credentials — everything else reads them where they
// already are — so staging MinIO's keys and sending them to Exoscale failed
// only here, and only after the archive had been made and encrypted:
//
//	Back-off restarting failed container upload
//
// while every other capture in the same export succeeded.
func stageVolumeSecret(
	ctx context.Context,
	c client.Client,
	name, namespace, tenantName, owner, sourceSecret string,
	external bool,
	extra map[string][]byte,
) error {
	if sourceSecret == "" {
		sourceSecret = backup.MinIOAdminSecret
	}
	source := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: sourceSecret, Namespace: kernelNamespace}, source); err != nil {
		return fmt.Errorf("read %s: %w", sourceSecret, err)
	}
	// An external destination's Secret carries only the keys: the endpoint is a
	// literal on the Job, from the policy, because it is the platform's own
	// storage whose address travels with its credentials.
	want := []string{"endpoint", "accessKey", "secretKey"}
	if external {
		want = []string{backup.DestinationAccessKeyField, backup.DestinationSecretKeyField}
	}
	data := map[string][]byte{}
	for _, k := range want {
		v, ok := source.Data[k]
		if !ok || len(v) == 0 {
			return fmt.Errorf("%s has no non-empty key %q", sourceSecret, k)
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
	// Where the bundle actually goes, as recorded when it was assigned.
	var credentialSecret string
	var external bool
	if b := export.Status.Bundle; b != nil {
		credentialSecret, external = b.CredentialSecret, b.Endpoint != ""
	}
	return stageVolumeSecret(ctx, r.Client,
		volumeUploadSecretName(export.Name), export.Namespace,
		tenantNameFromNamespace(export.Namespace), export.Name,
		credentialSecret, external, extra)
}

func (r *TenantExportReconciler) discardVolumeUploadSecret(ctx context.Context, export *gentianov1alpha1.TenantExport) error {
	return discardStagedSecret(ctx, r.Client, volumeUploadSecretName(export.Name), export.Namespace)
}

// ensureRestoreVolumeSecret stages the restore's copy into the tenant
// namespace. Only the storage credentials: the decryption key material already
// lives in the tenant namespace, where the operator was told to find it.
//
// From wherever the bundle is, which for a restore is the more important half:
// a bundle written to an external destination can only be read back with that
// destination's keys, and a cluster rebuilt from one has no other copy.
func (r *TenantRestoreReconciler) ensureRestoreVolumeSecret(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
) error {
	var credentialSecret string
	var external bool
	if b := restore.Status.Bundle; b != nil {
		credentialSecret, external = b.CredentialSecret, b.Endpoint != ""
	}
	return stageVolumeSecret(ctx, r.Client,
		restoreVolumeSecretName(restore.Name), restore.Namespace,
		tenantNameFromNamespace(restore.Namespace), restore.Name,
		credentialSecret, external, nil)
}

func (r *TenantRestoreReconciler) discardRestoreVolumeSecret(ctx context.Context, restore *gentianov1alpha1.TenantRestore) error {
	return discardStagedSecret(ctx, r.Client, restoreVolumeSecretName(restore.Name), restore.Namespace)
}
