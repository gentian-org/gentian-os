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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	conditionStorageReady = "StorageReady"
	minioAdminSecret      = backup.MinIOAdminSecret
	storageRequeueAfter   = 2 * time.Second
)

// ensureStorage provisions per-app MinIO S3 buckets declared via AppProfile
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
// and, when accessKey/secretKey are supplied (Seeder enabled), a scoped MinIO
// user + policy for the app. The operator seeds the credential into OpenBao and
// injects the exact key pair here, so tenant workloads never need OpenBao access
// to provision storage (SEC-1).
func makeS3BucketJob(tenant *gentianov1alpha1.Tenant, appName, accessKey, secretKey string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	// The container that makes a bucket, its user and its policy is the
	// inventory's: a restore runs the same one before it writes a bucket's
	// objects back.
	container := backup.ObjectStorageProvisionContainer("create-bucket", s3BucketName(tenant, appName), accessKey, secretKey)
	return &batchv1.Job{
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
}

// minioEndpoint returns the MinIO S3 endpoint from the kernel minio-admin
// Secret. Best-effort: returns "" when the Secret is unavailable so seeding
// still records the remaining S3 fields.
func (r *TenantReconciler) minioEndpoint(ctx context.Context) string {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: minioAdminSecret, Namespace: s3Namespace}, secret); err != nil {
		return ""
	}
	return string(secret.Data["endpoint"])
}

// makeS3BucketDeleteJob removes a bucket with the user and policy made for
// it: the Job the purge of one app runs too.
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
		Endpoint: r.minioEndpoint(ctx),
		Bucket:   s3BucketName(tenant, appName),
		Region:   "us-east-1",
	})
	if err != nil {
		return "", "", fmt.Errorf("seed s3 for %s: %w", appName, err)
	}
	return creds.AccessKey, creds.SecretKey, nil
}

// objectStorageProvisioner is the container that makes an app's bucket, its
// user and its policy, holding the key pair the vault holds for the app:
// what install's bucket Job runs, for a restore to run first.
func (r *TenantReconciler) objectStorageProvisioner(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName, name string) (corev1.Container, error) {
	accessKey, secretKey, err := r.seedObjectStorage(ctx, tenant, appName)
	if err != nil {
		return corev1.Container{}, err
	}
	return backup.ObjectStorageProvisionContainer(name, s3BucketName(tenant, appName), accessKey, secretKey), nil
}
