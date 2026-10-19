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
	"maps"
	"reflect"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// jobSecretPasswordKey is the key of the Secret a provisioning Job reads an
// app's store password from (jobSecret).
const jobSecretPasswordKey = "password"

// jobSecretClientSecretKey is the key of the Secret a Keycloak client Job
// reads an app's OIDC client secret from (jobSecret).
const jobSecretClientSecretKey = "client-secret"

// clientSecretSecretData is what the Secret handing an app's OIDC client
// secret to its Keycloak client Job holds.
func clientSecretSecretData(clientSecret string) map[string][]byte {
	return map[string][]byte{jobSecretClientSecretKey: []byte(clientSecret)}
}

// passwordSecretData is what the Secret handing an app's store password to
// its provisioning Job holds.
func passwordSecretData(password string) map[string][]byte {
	return map[string][]byte{jobSecretPasswordKey: []byte(password)}
}

// jobSecret is the Secret a provisioning Job reads a credential from: named
// as the Job, in its namespace, and owned by it, so that it is collected
// with the Job and lives no longer than the step that needs it. job must be
// the Job as the cluster has it: its UID is what the ownership names.
//
// A credential reaches a provisioning Job this way and never as a value in
// its spec: the Jobs' specs travel through the provisioning ConfigMap and the
// Objects Crossplane composes from it, and are readable wherever a Job is.
func jobSecret(job *batchv1.Job, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: job.Namespace,
			Labels:    maps.Clone(job.Labels),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "Job",
				Name:       job.Name,
				UID:        job.UID,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// ensureJobSecret writes desired, the Secret a Job reads a credential from.
// It is written again when the Job is a new one (a retired Job's Secret went
// with it) or the credential changed, and left alone otherwise.
func ensureJobSecret(ctx context.Context, c client.Client, job *batchv1.Job, desired *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("hand the credential to Job %s/%s: %w", job.Namespace, job.Name, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) &&
		reflect.DeepEqual(existing.Data, desired.Data) {
		return nil
	}
	existing.OwnerReferences = desired.OwnerReferences
	existing.Labels = desired.Labels
	existing.Data = desired.Data
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("hand the credential to Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return nil
}

// provisioningJobSecrets are the credentials the Jobs in a tenant's
// provisioning manifests are to be handed, by Job: what each Job's Secret
// holds -- a store's password or key pair, an OIDC client's secret. They are
// not in the Jobs.
type provisioningJobSecrets map[types.NamespacedName]map[string][]byte

// ensureProvisioningJobSecrets hands each Job Crossplane has made from the
// provisioning manifests the credential it provisions an app's store or
// OIDC client with. A Job not made yet is passed over: the operator watches
// the Jobs in the platform namespaces, and the one Crossplane makes brings
// the tenant back here. Until its Secret is there the Job's pod waits, which
// kubelet retries.
//
// A Job that has succeeded needs its Secret no more, and its Secret is
// removed: these Jobs stay once they have run (keepFinishedJobs), and the
// credential is not to stay with them. A Job that runs again is a new Job,
// and is handed the credential again.
func (r *TenantReconciler) ensureProvisioningJobSecrets(ctx context.Context, handoffs provisioningJobSecrets) error {
	for key, data := range handoffs {
		job := &batchv1.Job{}
		err := r.Get(ctx, key, job)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if job.DeletionTimestamp != nil {
			continue
		}
		if jobIsComplete(job) {
			if err := deleteJobSecret(ctx, r.Client, job); err != nil {
				return err
			}
			continue
		}
		if err := ensureJobSecret(ctx, r.Client, job, jobSecret(job, data)); err != nil {
			return err
		}
	}
	return nil
}

// deleteJobSecret removes the Secret a Job read its credential from, once the
// Job is done with it. Only the Secret this Job owns is removed: one of the
// same name owned by an earlier Job is that Job's, and goes with it.
func deleteJobSecret(ctx context.Context, c client.Client, job *batchv1.Job) error {
	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, secret)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	owned := false
	for _, ref := range secret.OwnerReferences {
		if ref.Kind == "Job" && ref.UID == job.UID {
			owned = true
		}
	}
	if !owned {
		return nil
	}
	if err := c.Delete(ctx, secret, client.Preconditions{UID: &secret.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove the credential of finished Job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return nil
}
