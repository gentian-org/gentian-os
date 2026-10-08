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
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// Where each unit of an export or a restore runs, and what is staged there.
//
// A unit runs in the namespace where the credential it works with already
// is: a database unit beside its database, with that server's administrator
// Secret; the realm unit in the identity namespace, with the identity
// provider's; a bucket unit and the manifest beside the object store; a
// volume unit in the tenant's namespace, because a claim mounts nowhere else.
// No administrator credential of a database or of the identity provider is
// copied anywhere.
//
// What a unit needs besides is the same for all of them: the credential the
// bundle is written or read with, and the key material it is encrypted or
// opened with. Those exist beside the object store only, so the controller
// stages one Secret holding both in each other namespace a run has a unit
// in, for exactly as long as the run lasts, and removes it on every exit
// path: completed, failed, or deleted.
//
// Until this, every unit but a volume's ran beside the object store and read
// the database and identity administrators' Secrets there by name. Since the
// namespaces were split by function those Secrets are beside their own
// services only: the pods could not be created, and no export or restore
// could run.

// runNamespaces are every namespace a run of this tenant can have a Job or a
// staged Secret in.
func runNamespaces(tenantNamespace string) []string {
	return dedupe(s3Namespace, postgresNamespace, mariadbNamespace, identityNamespace, tenantNamespace)
}

// stagedSecretName names an export's staged copy; the same name in every
// namespace it is staged in.
func stagedSecretName(tenantName, exportName string) string {
	return exportJobName(tenantName, exportName, "run", "creds")
}

// restoreStagedSecretName names a restore's staged copy. A distinct unit
// suffix, because an export and a restore may share a name.
func restoreStagedSecretName(tenantName, restoreName string) string {
	return exportJobName(tenantName, restoreName, "run", "rcreds")
}

// unitNamespaces are the namespaces a set of units runs in that need a staged
// copy: every one but the object store's, where the originals are.
func unitNamespaces(units []captureUnit) []string {
	var out []string
	for _, unit := range units {
		if ns := unit.Job.Namespace; ns != s3Namespace && !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

// putRunSecret creates a Secret of a run, or brings one of the same run up to
// date. A Secret of that name that is not this run's is left alone and is an
// error: it holds another run's credentials or key.
func putRunSecret(ctx context.Context, c client.Client, desired *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := c.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	case err != nil:
		return err
	}
	if !runObjectIsOurs(existing.Labels, desired.Labels[tenantLabel], desired.Labels[backup.ExportLabel]) {
		return fmt.Errorf("a Secret named %s in %s belongs to another run (tenant %q, run %q); it was left as it is",
			desired.Name, desired.Namespace, existing.Labels[tenantLabel], existing.Labels[backup.ExportLabel])
	}
	if equality.Semantic.DeepEqual(existing.Data, desired.Data) {
		return nil
	}
	existing.Data = desired.Data
	return c.Update(ctx, existing)
}

// runSecret is the shape of every Secret a run stages.
func runSecret(name, namespace, tenantName, run string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				tenantLabel:        tenantName,
				managedByLabel:     managedByValue,
				backup.ExportLabel: run,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// stageRunSecret writes the staged copy into one namespace: the credentials
// the bundle is reached with, plus any extra keys (a passphrase for an
// encrypting export, the key a restore opens the bundle with). Idempotent.
//
// sourceSecret names where those credentials come from, beside the object
// store. For a bundle going to the platform's own storage that is the MinIO
// admin Secret; for an external destination it is the Secret ESO materialised
// from the destination's credential, and the two are different accounts on
// different systems.
//
// Getting that wrong is invisible until the upload runs: staging MinIO's
// keys and sending them to an external store failed only in the units that
// read the staged copy, and only after the archive had been made and
// encrypted:
//
//	Back-off restarting failed container upload
//
// while every other capture in the same export succeeded.
func stageRunSecret(
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
	if err := c.Get(ctx, types.NamespacedName{Name: sourceSecret, Namespace: s3Namespace}, source); err != nil {
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
	return putRunSecret(ctx, c, runSecret(name, namespace, tenantName, owner, data))
}

func discardStagedSecret(ctx context.Context, c client.Client, name, namespace string) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := c.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// discardStagedIn removes a run's staged copy from every namespace it can be
// in. Every namespace is tried; the first error is returned.
func discardStagedIn(ctx context.Context, c client.Client, name, tenantNamespace string) error {
	var first error
	for _, ns := range runNamespaces(tenantNamespace) {
		if ns == s3Namespace {
			continue
		}
		if err := discardStagedSecret(ctx, c, name, ns); err != nil && first == nil {
			first = fmt.Errorf("remove the staged Secret %s from %s: %w", name, ns, err)
		}
	}
	return first
}

// placeExportUnit moves a unit's parameters to the namespace it runs in and
// points it at the copy staged there. Beside the object store nothing is
// staged: the originals are there.
func placeExportUnit(p backup.JobParams, namespace, staged string) backup.JobParams {
	if namespace == s3Namespace {
		return p
	}
	p.Namespace = namespace
	p.UploadCredentialsSecret = staged
	if p.Encryption.Mode == gentianov1alpha1.ExportEncryptionPassphrase {
		p.Encryption.PassphraseSecret = staged
	}
	return p
}

// ensureStagedSecret stages the export's copy into one namespace.
func (r *TenantExportReconciler) ensureStagedSecret(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	namespace string,
	enc backup.Encryption,
) error {
	tenantName := tenantNameFromNamespace(export.Namespace)
	extra := map[string][]byte{}
	if enc.Mode == gentianov1alpha1.ExportEncryptionPassphrase {
		pp := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: enc.PassphraseSecret, Namespace: s3Namespace}, pp); err != nil {
			return fmt.Errorf("read the passphrase to stage it in %s: %w", namespace, err)
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
	return stageRunSecret(ctx, r.Client,
		stagedSecretName(tenantName, export.Name), namespace,
		tenantName, export.Name,
		credentialSecret, external, extra)
}

// stageFor stages the export's copy wherever these units need one.
func (r *TenantExportReconciler) stageFor(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	units []captureUnit,
	enc backup.Encryption,
) error {
	for _, ns := range unitNamespaces(units) {
		if err := r.ensureStagedSecret(ctx, export, ns, enc); err != nil {
			return fmt.Errorf("stage credentials in %s: %w", ns, err)
		}
	}
	return nil
}

func (r *TenantExportReconciler) discardStagedSecrets(ctx context.Context, export *gentianov1alpha1.TenantExport) error {
	return discardStagedIn(ctx, r.Client,
		stagedSecretName(tenantNameFromNamespace(export.Namespace), export.Name), export.Namespace)
}

// placeRestoreUnit moves a unit's parameters and its key to the namespace it
// runs in. Beside the object store both are already there. In the tenant's
// own namespace the key is read from the Secret the restore names, which the
// spec requires to be there; everywhere else it travels in the staged copy.
func (r *TenantRestoreReconciler) placeRestoreUnit(
	restore *gentianov1alpha1.TenantRestore,
	p backup.JobParams,
	d backup.Decryption,
	namespace string,
) (backup.JobParams, backup.Decryption) {
	if namespace == s3Namespace {
		return p, d
	}
	staged := restoreStagedSecretName(tenantNameFromNamespace(restore.Namespace), restore.Name)
	p.Namespace = namespace
	p.UploadCredentialsSecret = staged
	if namespace == restore.Namespace {
		if dec := restore.Spec.Decryption; dec != nil {
			if d.Mode == gentianov1alpha1.ExportEncryptionPassphrase && dec.PassphraseSecretRef != nil {
				d.SecretName = dec.PassphraseSecretRef.Name
			} else if dec.IdentitySecretRef != nil {
				d.SecretName = dec.IdentitySecretRef.Name
			}
		}
		return p, d
	}
	if d.SecretName != "" {
		d.SecretName = staged
	}
	return p, d
}

// ensureRestoreStagedSecret stages the restore's copy into one namespace: the
// storage credentials, and outside the tenant's own namespace the key the
// bundle is opened with.
//
// From wherever the bundle is, which for a restore is the more important half:
// a bundle written to an external destination can only be read back with that
// destination's keys, and a cluster rebuilt from one has no other copy.
func (r *TenantRestoreReconciler) ensureRestoreStagedSecret(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	namespace string,
	d backup.Decryption,
) error {
	tenantName := tenantNameFromNamespace(restore.Namespace)
	extra := map[string][]byte{}
	if namespace != restore.Namespace && d.SecretName != "" {
		key := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: d.SecretName, Namespace: s3Namespace}, key); err != nil {
			return fmt.Errorf("read the decryption key to stage it in %s: %w", namespace, err)
		}
		value, ok := key.Data[d.SecretKey]
		if !ok || len(value) == 0 {
			return fmt.Errorf("the staged decryption Secret %q has no non-empty key %q", d.SecretName, d.SecretKey)
		}
		extra[d.SecretKey] = value
	}
	var credentialSecret string
	var external bool
	if b := restore.Status.Bundle; b != nil {
		credentialSecret, external = b.CredentialSecret, b.Endpoint != ""
	}
	return stageRunSecret(ctx, r.Client,
		restoreStagedSecretName(tenantName, restore.Name), namespace,
		tenantName, restore.Name,
		credentialSecret, external, extra)
}

// stageFor stages the restore's copy wherever these units need one.
func (r *TenantRestoreReconciler) stageFor(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	units []captureUnit,
	d backup.Decryption,
) error {
	for _, ns := range unitNamespaces(units) {
		if err := r.ensureRestoreStagedSecret(ctx, restore, ns, d); err != nil {
			return fmt.Errorf("stage credentials in %s: %w", ns, err)
		}
	}
	return nil
}

func (r *TenantRestoreReconciler) discardRestoreStagedSecrets(ctx context.Context, restore *gentianov1alpha1.TenantRestore) error {
	return discardStagedIn(ctx, r.Client,
		restoreStagedSecretName(tenantNameFromNamespace(restore.Namespace), restore.Name), restore.Namespace)
}
