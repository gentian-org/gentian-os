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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	conditionStorageReady = "StorageReady"
	storageAdminSecret    = backup.StorageAdminSecret
	storageRequeueAfter   = 2 * time.Second
)

// ensureStorage provisions per-app S3 buckets declared via ComponentProfile
// ServiceRequirements.Storage.S3.
func (r *TenantReconciler) ensureStorage(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	s3Apps, err := r.collectStorageApps(ctx, tenant, CollectForProvision)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(s3Apps) == 0 {
		r.setCondition(tenant, conditionStorageReady, metav1.ConditionTrue,
			"NoStorageRequired", "No apps require storage provisioning")
		return ctrl.Result{}, nil
	}

	allDone := true
	var pendingJobs []string
	for _, appName := range s3Apps {
		done, err := r.ensureS3BucketJob(ctx, tenant, appName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure S3 bucket Job for app %s: %w", appName, err)
		}
		if !done {
			pendingJobs = append(pendingJobs, s3BucketJobName(tenant.Name, appName))
			allDone = false
		}
	}

	if !allDone {
		r.setCondition(tenant, conditionStorageReady, metav1.ConditionFalse,
			"Provisioning", "Waiting for storage resources to be ready")
		return r.requeueForPendingJob(ctx, tenant.Name, pendingJobs...), nil
	}

	r.setCondition(tenant, conditionStorageReady, metav1.ConditionTrue,
		"Provisioned", "All storage resources are ready")
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) collectStorageApps(ctx context.Context, tenant *gentianov1alpha1.Tenant, mode AppCollectionMode) ([]string, error) {
	return r.collectKernelApps(ctx, tenant, mode, matchS3Profile, func(tenantName string) string {
		return s3BucketJobName(tenantName, "")
	}, func(p backup.Provisioned) bool { return p.Has(backup.KindObjectStorage) })
}

func (r *TenantReconciler) ensureS3BucketJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, s3BucketJobName(tenant.Name, appName))
}

func (r *TenantReconciler) deleteStorage(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}

	s3Apps, err := r.collectStorageApps(ctx, tenant, CollectForDelete)
	if err != nil {
		return err
	}
	// The backup bucket goes with the rest. It is not an app's, so it is
	// never in the inventory a profile can reach, and it is named here as
	// its own unit: a purge that leaves the backups behind has not purged.
	// keepBundles is the one exception, for a tenant handed a copy whose
	// provider keeps one under contract.
	if !tenant.KeepsBundles() {
		s3Apps = append(s3Apps, backupBucketUnit)
	}
	return r.ensureDeleteJobs(ctx, s3Namespace, tenant, s3Apps, s3BucketDeleteJobName, makeS3BucketDeleteJob)
}

// backupBucketUnit is the pseudo-app name the backup bucket's delete Job
// carries. backup.BackupBucket and backup.S3Bucket agree on the bucket this
// names, which is what lets one Job constructor serve both.
const backupBucketUnit = "gentian-backup"

// makeS3BucketJob builds a kernel-namespace Job that creates the per-app bucket
// and, when withKey is set (Seeder enabled), an identity for the app scoped
// to that bucket. The operator seeds the credential into OpenBao and hands
// the exact key pair over in a Secret the Job owns (ensureProvisioningJobSecrets),
// so tenant workloads never need OpenBao access to provision storage (SEC-1)
// and the key is in no Job spec, no ConfigMap and no composed Object.
func makeS3BucketJob(tenant *gentianov1alpha1.Tenant, appName string, withKey bool, opts backup.ObjectStorageOptions) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	keySecret := ""
	if withKey {
		keySecret = s3BucketJobName(tenant.Name, appName)
	}
	// The container that makes a bucket and its identity is the
	// inventory's: a restore runs the same one before it writes a bucket's
	// objects back.
	container := backup.ObjectStorageProvisionContainerWith("create-bucket", s3BucketName(tenant, appName), keySecret, opts)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s3BucketJobName(tenant.Name, appName),
			Namespace: s3Namespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
	// The container administers the store through the master and the
	// filer, which admit this pod by its label and its client certificate.
	backup.ObjectStoreShellPod(&job.Spec.Template)
	return job
}

// storageEndpoint returns the object store's S3 endpoint from its
// administrator's Secret. Best-effort: returns "" when the Secret is unavailable so seeding
// still records the remaining S3 fields.
func (r *TenantReconciler) storageEndpoint(ctx context.Context) string {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: storageAdminSecret, Namespace: s3Namespace}, secret); err != nil {
		return ""
	}
	return string(secret.Data["endpoint"])
}

// makeS3BucketDeleteJob removes a bucket with the identity made for it: the Job the purge of one app runs too.
func makeS3BucketDeleteJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	return backup.ObjectStorageDestroyJob(tenant, appName, backup.DestroyInTheBackground)
}

func s3BucketName(tenant *gentianov1alpha1.Tenant, appName string) string {
	return backup.S3Bucket(tenant, appName)
}

func s3BucketJobName(tenantName, appName string) string {
	return fmt.Sprintf("s3-bucket-%s-%s", tenantName, appName)
}

func s3BucketDeleteJobName(tenantName, appName string) string {
	return backup.ObjectStorageDestroyJobName(tenantName, appName)
}

// seedObjectStorage seeds the key pair an app reads its bucket with, and
// returns it. Empty when the operator has no vault: the bucket is then made
// without a user, as it always was there.
func (r *TenantReconciler) seedObjectStorage(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) (accessKey, secretKey string, err error) {
	if r.Seeder == nil {
		return "", "", nil
	}
	creds, err := r.Seeder.SeedS3(ctx, tenant.Name, appName, secrets.S3Creds{
		Endpoint: r.storageEndpoint(ctx),
		Bucket:   s3BucketName(tenant, appName),
		Region:   "us-east-1",
	})
	if err != nil {
		return "", "", fmt.Errorf("seed s3 for %s: %w", appName, err)
	}
	return creds.AccessKey, creds.SecretKey, nil
}

// objectStorageProvisioner is the container that makes an app's bucket and
// its identity, for a restore to run first in the Job named jobName, and the
// key pair the vault holds for the app, which that Job's pod reads from the
// Secret of the Job's name (backup.ObjectStorageKeySecret). The key pair is
// empty, and the container makes the bucket alone, on a cluster with no
// vault.
func (r *TenantReconciler) objectStorageProvisioner(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName, name, jobName string) (corev1.Container, *objectStoreKey, error) {
	accessKey, secretKey, err := r.seedObjectStorage(ctx, tenant, appName)
	if err != nil {
		return corev1.Container{}, nil, err
	}
	opts, err := r.objectStorageOptions(ctx, appName)
	if err != nil {
		return corev1.Container{}, nil, err
	}
	if accessKey == "" || secretKey == "" {
		return backup.ObjectStorageProvisionContainerWith(name, s3BucketName(tenant, appName), "", opts), nil, nil
	}
	return backup.ObjectStorageProvisionContainerWith(name, s3BucketName(tenant, appName), jobName, opts),
		&objectStoreKey{accessKey: accessKey, secretKey: secretKey}, nil
}

// objectStorageOptions are what the app's profile asks of its bucket
// (requires.services.s3). A profile that cannot be read asks for nothing
// more than a bucket only where it is gone, which is a restore's or a
// purge's case; an install reads it.
func (r *TenantReconciler) objectStorageOptions(ctx context.Context, appName string) (backup.ObjectStorageOptions, error) {
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, client.ObjectKey{Name: appName}, profile); err != nil {
		if apierrors.IsNotFound(err) {
			return backup.ObjectStorageOptions{}, nil
		}
		return backup.ObjectStorageOptions{}, fmt.Errorf("get ComponentProfile %s: %w", appName, err)
	}
	if !matchS3Profile(profile) {
		return backup.ObjectStorageOptions{}, nil
	}
	return backup.ObjectStorageOptions{ObjectLock: profile.Services().Storage.S3.ObjectLock}, nil
}

// objectStoreKey is an app's key pair on its way to the Secret a Job that
// provisions the app's bucket reads it from.
type objectStoreKey struct {
	accessKey, secretKey string
}

// ensureObjectStoreKeySecret hands a key pair to the Job that provisions a
// bucket: the Secret of the Job's name, owned by the Job as the cluster has
// it (ensureJobSecret).
func ensureObjectStoreKeySecret(ctx context.Context, c client.Client, job *batchv1.Job, key objectStoreKey) error {
	return ensureJobSecret(ctx, c, job, backup.ObjectStorageKeySecret(job, key.accessKey, key.secretKey))
}
