/*
Copyright 2026 The Gentian Authors.

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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	conditionStorageReady     = "StorageReady"
	minioProvisionerImage     = "minio/mc:latest"
	minioAdminSecret          = "minio-admin"
	nextcloudProvisionerImage = "curlimages/curl:8.7.1"
	nextcloudAdminSecret      = "nextcloud-admin"
	storageRequeueAfter       = 30 * time.Second
)

// ensureStorage provisions per-app MinIO S3 buckets and per-tenant Nextcloud
// groups. Each pathway creates a Job in the kernel namespace; StorageReady is
// set to True once all Jobs complete.
func (r *TenantReconciler) ensureStorage(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	s3Apps, ncApps, err := r.collectStorageApps(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(s3Apps) == 0 && len(ncApps) == 0 {
		r.setCondition(tenant, conditionStorageReady, metav1.ConditionTrue,
			"NoStorageRequired", "No apps require storage provisioning")
		return ctrl.Result{}, nil
	}

	allDone := true

	// --- S3 buckets (MinIO) ---
	for _, appName := range s3Apps {
		done, err := r.ensureS3BucketJob(ctx, tenant, appName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure S3 bucket Job for app %s: %w", appName, err)
		}
		if !done {
			allDone = false
		}
	}

	// --- Nextcloud group (WebDAV) ---
	// A single group covers all WebDAV-requiring apps in the same tenant.
	if len(ncApps) > 0 {
		done, err := r.ensureNextcloudGroupJob(ctx, tenant)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Nextcloud group Job: %w", err)
		}
		if !done {
			allDone = false
		}
	}

	if !allDone {
		r.setCondition(tenant, conditionStorageReady, metav1.ConditionFalse,
			"Provisioning", "Waiting for storage resources to be ready")
		return ctrl.Result{RequeueAfter: storageRequeueAfter}, nil
	}

	r.setCondition(tenant, conditionStorageReady, metav1.ConditionTrue,
		"Provisioned", "All storage resources are ready")
	return ctrl.Result{}, nil
}

// collectStorageApps returns two slices: app profile names that require S3 and
// app profile names that require WebDAV file access.
func (r *TenantReconciler) collectStorageApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) (s3Apps, ncApps []string, err error) {
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.KernelRequirements == nil || profile.Spec.KernelRequirements.Storage == nil {
			continue
		}
		stor := profile.Spec.KernelRequirements.Storage
		if stor.S3 != nil {
			s3Apps = append(s3Apps, app.Profile)
		}
		if stor.Files != nil {
			ncApps = append(ncApps, app.Profile)
		}
	}
	return s3Apps, ncApps, nil
}

// ensureS3BucketJob creates (or checks) the MinIO bucket setup Job for one app.
// Returns true when the Job has completed successfully.
func (r *TenantReconciler) ensureS3BucketJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) (bool, error) {
	jobName := s3BucketJobName(tenant.Name, appName)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, makeS3BucketJob(tenant, appName))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
}

// ensureNextcloudGroupJob creates (or checks) the Nextcloud group setup Job.
// One group per tenant covers all WebDAV-requiring apps. Returns true when done.
func (r *TenantReconciler) ensureNextcloudGroupJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, error) {
	jobName := nextcloudGroupJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, makeNextcloudGroupJob(tenant))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
}

// deleteStorage handles storage cleanup on tenant deletion.
// DeletionPolicy=Delete: creates delete Jobs to remove the MinIO bucket and
// Nextcloud group. DeletionPolicy=Retain: no-op — data is preserved.
func (r *TenantReconciler) deleteStorage(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}

	s3Apps, ncApps, err := r.collectStorageApps(ctx, tenant)
	if err != nil {
		return err
	}

	for _, appName := range s3Apps {
		deleteJobName := s3BucketDeleteJobName(tenant.Name, appName)
		existing := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: deleteJobName, Namespace: kernelNamespace}, existing); errors.IsNotFound(err) {
			if err := r.Create(ctx, makeS3BucketDeleteJob(tenant, appName)); err != nil {
				return fmt.Errorf("create S3 delete Job for %s: %w", appName, err)
			}
		} else if err != nil {
			return err
		}
	}

	if len(ncApps) > 0 {
		deleteJobName := nextcloudGroupDeleteJobName(tenant.Name)
		existing := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: deleteJobName, Namespace: kernelNamespace}, existing); errors.IsNotFound(err) {
			if err := r.Create(ctx, makeNextcloudGroupDeleteJob(tenant)); err != nil {
				return fmt.Errorf("create Nextcloud delete Job: %w", err)
			}
		} else if err != nil {
			return err
		}
	}

	return nil
}

// --- Job constructors --------------------------------------------------------

// makeS3BucketJob creates a MinIO mc Job that provisions a per-app S3 bucket.
func makeS3BucketJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	ttl := int32(3600)
	bucket := s3BucketName(tenant, appName)
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
					Containers: []corev1.Container{
						minioContainer("create-bucket", bucket, minioSetupScript(bucket)),
					},
				},
			},
		},
	}
}

// makeS3BucketDeleteJob creates a MinIO mc Job that removes the per-app S3 bucket.
func makeS3BucketDeleteJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	ttl := int32(3600)
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

// makeNextcloudGroupJob creates a curl Job that provisions a Nextcloud group via
// the OCS API for all WebDAV-requiring apps in the tenant.
func makeNextcloudGroupJob(tenant *gentianov1alpha1.Tenant) *batchv1.Job {
	ttl := int32(3600)
	group := nextcloudGroupName(tenant.Name)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nextcloudGroupJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						nextcloudContainer("create-group", group, nextcloudCreateGroupScript(group)),
					},
				},
			},
		},
	}
}

// makeNextcloudGroupDeleteJob creates a curl Job that removes the tenant's
// Nextcloud group via the OCS API.
func makeNextcloudGroupDeleteJob(tenant *gentianov1alpha1.Tenant) *batchv1.Job {
	ttl := int32(3600)
	group := nextcloudGroupName(tenant.Name)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nextcloudGroupDeleteJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						nextcloudContainer("delete-group", group, nextcloudDeleteGroupScript(group)),
					},
				},
			},
		},
	}
}

// --- Container constructors --------------------------------------------------

// minioContainer returns a Container running minio/mc with admin credentials
// from the minio-admin Secret. The bucket name and script are passed via env/command.
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
			// Bucket name as a plain literal — validated to [a-z0-9-] by s3BucketName.
			{Name: "BUCKET_NAME", Value: bucket},
		},
	}
}

// nextcloudContainer returns a Container running curlimages/curl to call the
// Nextcloud OCS API with admin credentials from the nextcloud-admin Secret.
func nextcloudContainer(name, group, script string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   nextcloudProvisionerImage,
		Command: []string{"/bin/sh", "-c", script},
		Env: []corev1.EnvVar{
			{
				Name: "NEXTCLOUD_URL",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: nextcloudAdminSecret},
						Key:                  "url",
					},
				},
			},
			{
				Name: "NEXTCLOUD_ADMIN_USER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: nextcloudAdminSecret},
						Key:                  "username",
					},
				},
			},
			{
				Name: "NEXTCLOUD_ADMIN_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: nextcloudAdminSecret},
						Key:                  "password",
					},
				},
			},
			// Group name as a plain literal — safe [a-z0-9-] chars.
			{Name: "NC_GROUP", Value: group},
		},
	}
}

// --- Shell scripts -----------------------------------------------------------

func minioSetupScript(bucket string) string {
	return fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
mc mb --ignore-existing "gentian/%s"
mc anonymous set none "gentian/%s"
echo "bucket %s ready"`, bucket, bucket, bucket)
}

func minioDeleteScript(bucket string) string {
	return fmt.Sprintf(`set -eu
mc alias set gentian "${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}"
mc rb --force "gentian/%s" 2>/dev/null || echo "bucket %s already gone"
echo "bucket %s removed"`, bucket, bucket, bucket)
}

func nextcloudCreateGroupScript(group string) string {
	return fmt.Sprintf(`set -eu
EXISTING=$(curl -sf -u "${NEXTCLOUD_ADMIN_USER}:${NEXTCLOUD_ADMIN_PASSWORD}" \
  "${NEXTCLOUD_URL}/ocs/v1.php/cloud/groups/%s" \
  -H "OCS-APIRequest: true" 2>/dev/null | grep -c "<id>%s</id>" || true)
if [ "${EXISTING}" = "0" ]; then
  curl -sf -u "${NEXTCLOUD_ADMIN_USER}:${NEXTCLOUD_ADMIN_PASSWORD}" \
    "${NEXTCLOUD_URL}/ocs/v1.php/cloud/groups" \
    -H "OCS-APIRequest: true" \
    -d "groupid=%s"
  echo "group %s created"
else
  echo "group %s already exists"
fi`, group, group, group, group, group)
}

func nextcloudDeleteGroupScript(group string) string {
	return fmt.Sprintf(`set -eu
curl -sf -u "${NEXTCLOUD_ADMIN_USER}:${NEXTCLOUD_ADMIN_PASSWORD}" \
  -X DELETE \
  "${NEXTCLOUD_URL}/ocs/v1.php/cloud/groups/%s" \
  -H "OCS-APIRequest: true" 2>/dev/null || echo "group %s already gone"
echo "group %s removed"`, group, group, group)
}

// --- Name and value helpers --------------------------------------------------

// s3BucketName returns the MinIO bucket name for a tenant + app.
// Uses spec.isolation.s3Prefix if set, else defaults to "{tenant-name}-".
// Characters are lowercased and hyphens preserved; invalid characters are replaced
// with hyphens to satisfy S3 bucket naming rules.
func s3BucketName(tenant *gentianov1alpha1.Tenant, appName string) string {
	prefix := tenant.Name + "-"
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.S3Prefix != "" {
		prefix = tenant.Spec.Isolation.S3Prefix
	}
	safe := func(s string) string {
		b := make([]byte, len(s))
		for i := 0; i < len(s); i++ {
			ch := s[i]
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				b[i] = ch
			} else if ch >= 'A' && ch <= 'Z' {
				b[i] = ch + 32 // lowercase
			} else {
				b[i] = '-'
			}
		}
		return string(b)
	}
	return safe(prefix) + safe(appName)
}

// nextcloudGroupName returns the Nextcloud group name for a tenant.
// Groups are per-tenant (not per-app) — all apps share the same group.
func nextcloudGroupName(tenantName string) string {
	return "tenant-" + tenantName
}

func s3BucketJobName(tenantName, appName string) string {
	return fmt.Sprintf("s3-bucket-%s-%s", tenantName, appName)
}

func s3BucketDeleteJobName(tenantName, appName string) string {
	return fmt.Sprintf("s3-delete-%s-%s", tenantName, appName)
}

func nextcloudGroupJobName(tenantName string) string {
	return fmt.Sprintf("nc-group-%s", tenantName)
}

func nextcloudGroupDeleteJobName(tenantName string) string {
	return fmt.Sprintf("nc-group-delete-%s", tenantName)
}
