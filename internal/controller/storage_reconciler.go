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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	conditionStorageReady = "StorageReady"
	minioProvisionerImage = "minio/mc:RELEASE.2025-04-03T17-07-56Z"
	minioAdminSecret      = "minio-admin"
	storageRequeueAfter   = 2 * time.Second
)

// ensureStorage provisions per-app MinIO S3 buckets declared via AppProfile
// KernelRequirements.Storage.S3.
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
	})
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
	return r.ensureDeleteJobs(ctx, tenant, s3Apps, s3BucketDeleteJobName, makeS3BucketDeleteJob)
}

// makeS3BucketJob builds a kernel-namespace Job that creates the per-app bucket
// and, when accessKey/secretKey are supplied (Seeder enabled), a scoped MinIO
// user + policy for the app. The operator seeds the credential into OpenBao and
// injects the exact key pair here, so tenant workloads never need OpenBao access
// to provision storage (SEC-1).
func makeS3BucketJob(tenant *gentianov1alpha1.Tenant, appName, accessKey, secretKey string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	bucket := s3BucketName(tenant, appName)
	container := minioContainer("create-bucket", bucket, minioSetupScript(bucket))
	if accessKey != "" && secretKey != "" {
		container.Env = append(container.Env,
			corev1.EnvVar{Name: "APP_ACCESS_KEY", Value: accessKey},
			corev1.EnvVar{Name: "APP_SECRET_KEY", Value: secretKey},
		)
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s3BucketJobName(tenant.Name, appName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
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
	if err := r.Get(ctx, types.NamespacedName{Name: minioAdminSecret, Namespace: kernelNamespace}, secret); err != nil {
		return ""
	}
	return string(secret.Data["endpoint"])
}

func makeS3BucketDeleteJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	bucket := s3BucketName(tenant, appName)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s3BucketDeleteJobName(tenant.Name, appName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       appName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						minioContainer("delete-bucket", bucket, minioDeleteScript(bucket)),
					},
				},
			},
		},
	}
}

func minioContainer(name, bucket, script string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   minioProvisionerImage,
		Command: []string{"/bin/sh", "-c", script},
		Env: []corev1.EnvVar{
			{
				Name: "MINIO_ENDPOINT",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: minioAdminSecret},
						Key:                  "endpoint",
					},
				},
			},
			{
				Name: "MINIO_ACCESS_KEY",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: minioAdminSecret},
						Key:                  "accessKey",
					},
				},
			},
			{
				Name: "MINIO_SECRET_KEY",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: minioAdminSecret},
						Key:                  "secretKey",
					},
				},
			},
			{Name: "BUCKET_NAME", Value: bucket},
		},
	}
}

func minioSetupScript(bucket string) string {
	return fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
mc mb --ignore-existing "gentian/%[1]s"
mc anonymous set none "gentian/%[1]s"
if [ -n "${APP_ACCESS_KEY:-}" ] && [ -n "${APP_SECRET_KEY:-}" ]; then
  mc admin user remove gentian "${APP_ACCESS_KEY}" >/dev/null 2>&1 || true
  mc admin user add gentian "${APP_ACCESS_KEY}" "${APP_SECRET_KEY}"
  printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::%[1]s","arn:aws:s3:::%[1]s/*"]}]}' > /tmp/policy.json
  mc admin policy rm gentian "${APP_ACCESS_KEY}-policy" >/dev/null 2>&1 || true
  mc admin policy create gentian "${APP_ACCESS_KEY}-policy" /tmp/policy.json
  mc admin policy attach gentian "${APP_ACCESS_KEY}-policy" --user "${APP_ACCESS_KEY}"
  echo "minio user for bucket %[1]s ready"
fi
echo "bucket %[1]s ready"`, bucket)
}

func minioDeleteScript(bucket string) string {
	return fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
mc rb --force "gentian/%s" 2>/dev/null || echo "bucket %s already gone"
echo "bucket %s removed"`, bucket, bucket, bucket)
}

func s3BucketName(tenant *gentianov1alpha1.Tenant, appName string) string {
	return backup.S3Bucket(tenant, appName)
}

func s3BucketJobName(tenantName, appName string) string {
	return fmt.Sprintf("s3-bucket-%s-%s", tenantName, appName)
}

func s3BucketDeleteJobName(tenantName, appName string) string {
	return fmt.Sprintf("s3-delete-%s-%s", tenantName, appName)
}
