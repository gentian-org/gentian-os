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
	"github.com/gentian-org/gentian-os/internal/meta"
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
	minioProvisionerImage     = "minio/mc:RELEASE.2025-04-03T17-07-56Z"
	minioAdminSecret          = "minio-admin"
	nextcloudProvisionerImage = "curlimages/curl:8.7.1"
	nextcloudPostgresImage    = "postgres:15"
	nextcloudAdminSecret      = "nextcloud-admin"
	storageRequeueAfter       = 2 * time.Second
)

// ensureStorage provisions per-app MinIO S3 buckets declared via AppProfile
// KernelRequirements.Storage.S3. Each pathway creates a Job in the kernel namespace;
// StorageReady is set to True once all Jobs complete.
// Note: per-tenant Nextcloud groups are provisioned via the manifest bridge
// (nc-group Job in jobs.json) and applied by tenant-default.
func (r *TenantReconciler) ensureStorage(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	if err := r.cleanupOrphanedNextcloudGroupJob(ctx, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup orphaned Nextcloud group Job: %w", err)
	}

	s3Apps, err := r.collectStorageApps(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(s3Apps) == 0 {
		r.setCondition(tenant, conditionStorageReady, metav1.ConditionTrue,
			"NoStorageRequired", "No apps require storage provisioning")
		return ctrl.Result{}, nil
	}

	allDone := true

	// --- S3 buckets (MinIO) ---
	for _, appName := range s3Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: appName}, profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("get AppProfile %s: %w", appName, err)
		}
		var done bool
		if appUsesCrossplaneS3Init(profile) {
			done, err = r.waitForTenantNamespaceJob(ctx, tenant, appCompositionInitJobName(appName, "s3-init"))
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("wait for s3-init Job for app %s: %w", appName, err)
			}
		} else {
			done, err = r.ensureS3BucketJob(ctx, tenant, appName)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("ensure S3 bucket Job for app %s: %w", appName, err)
			}
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

// collectStorageApps returns the app profile names that require an S3 bucket.
// Per-tenant Nextcloud groups are provisioned via the manifest bridge (nc-group Job).
func (r *TenantReconciler) collectStorageApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) (s3Apps []string, err error) {
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.KernelRequirements == nil || profile.Spec.KernelRequirements.Storage == nil {
			continue
		}
		if profile.Spec.KernelRequirements.Storage.S3 != nil {
			s3Apps = append(s3Apps, app.Profile)
		}
	}
	return s3Apps, nil
}

// ensureS3BucketJob waits for the Crossplane-owned MinIO bucket Job.
func (r *TenantReconciler) ensureS3BucketJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, s3BucketJobName(tenant.Name, appName))
}

// deleteStorage handles storage cleanup on tenant deletion.
// DeletionPolicy=Delete: creates delete Jobs to remove the MinIO bucket and
// Nextcloud group. DeletionPolicy=Retain: no-op — data is preserved.
func (r *TenantReconciler) deleteStorage(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}

	s3Apps, err := r.collectStorageAppsForDelete(ctx, tenant)
	if err != nil {
		return err
	}

	pending := false
	for _, appName := range s3Apps {
		deleteJobName := s3BucketDeleteJobName(tenant.Name, appName)
		existing := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: deleteJobName, Namespace: kernelNamespace}, existing); errors.IsNotFound(err) {
			if err := r.Create(ctx, makeS3BucketDeleteJob(tenant, appName)); err != nil {
				return fmt.Errorf("create S3 delete Job for %s: %w", appName, err)
			}
			pending = true
		} else if err != nil {
			return err
		} else if !jobIsComplete(existing) {
			pending = true
		}
	}

	// Nextcloud group delete Job (manifest bridge owns nc-group provisioning).
	if r.nextcloudKernelAvailable(ctx) {
		ncDeleteJobName := nextcloudGroupDeleteJobName(tenant.Name)
		existingNC := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: ncDeleteJobName, Namespace: kernelNamespace}, existingNC); errors.IsNotFound(err) {
			if err := r.Create(ctx, makeNextcloudGroupDeleteJob(tenant)); err != nil {
				return fmt.Errorf("create Nextcloud delete Job: %w", err)
			}
			pending = true
		} else if err != nil {
			return err
		} else if !jobIsComplete(existingNC) {
			pending = true
		}
	}

	if pending {
		return errDeleteJobPending
	}
	return nil
}

// --- Job constructors --------------------------------------------------------

// makeS3BucketJob creates a MinIO mc Job that provisions a per-app S3 bucket.
func makeS3BucketJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
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

// nextcloudKernelAvailable reports whether the shared Nextcloud kernel service is
// deployed (nextcloud-admin Secret present in platform-kernel).
func (r *TenantReconciler) nextcloudKernelAvailable(ctx context.Context) bool {
	key := types.NamespacedName{Name: nextcloudAdminSecret, Namespace: kernelNamespace}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, key, secret); err == nil {
		return true
	}
	if r.APIReader != nil {
		return r.APIReader.Get(ctx, key, secret) == nil
	}
	return false
}

// cleanupOrphanedNextcloudGroupJob removes nc-group Jobs when Nextcloud is not deployed.
func (r *TenantReconciler) cleanupOrphanedNextcloudGroupJob(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if r.nextcloudKernelAvailable(ctx) {
		return nil
	}
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: nextcloudGroupJobName(tenant.Name), Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	prop := metav1.DeletePropagationBackground
	return r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
}

// makeNextcloudGroupJob creates a curl Job that provisions a Nextcloud group via
// the OCS API for all WebDAV-requiring apps in the tenant.
func makeNextcloudGroupJob(tenant *gentianov1alpha1.Tenant) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
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

// makeNextcloudGroupDeleteJob creates a two-container Job that:
//  1. (init) queries the Nextcloud Postgres DB to find all NC users whose LDAP DN
//     is under the tenant's OU, writing their IDs to a shared emptyDir volume.
//  2. (main) deletes each user via the OCS API and then removes the tenant group.
//
// Using DB-based lookup avoids relying on per-tenant NC group membership, which
// is not used for user assignment (users are auto-assigned to the cross-tenant
// managed-by-attribute-Fileshare group via LDAP attributes instead).
func makeNextcloudGroupDeleteJob(tenant *gentianov1alpha1.Tenant) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
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
					Volumes: []corev1.Volume{
						{
							Name:         "shared",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
					InitContainers: []corev1.Container{
						nextcloudUserLookupContainer(tenant.Name),
					},
					Containers: []corev1.Container{
						nextcloudDeleteContainer(group),
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

// nextcloudUserLookupContainer returns a postgres init Container that queries the
// Nextcloud DB for all users whose LDAP DN is under the tenant OU and writes
// their NC user IDs (one per line) to /shared/users.txt.
func nextcloudUserLookupContainer(tenantName string) corev1.Container {
	return corev1.Container{
		Name:    "lookup-users",
		Image:   nextcloudPostgresImage,
		Command: []string{"/bin/sh", "-c", nextcloudUserLookupScript(tenantName)},
		Env: []corev1.EnvVar{
			{
				Name: "NC_DB_HOST",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: nextcloudAdminSecret},
						Key:                  "dbhost",
					},
				},
			},
			{
				Name: "NC_DB_NAME",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: nextcloudAdminSecret},
						Key:                  "dbname",
					},
				},
			},
			{
				Name: "NC_DB_USER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: nextcloudAdminSecret},
						Key:                  "dbuser",
					},
				},
			},
			{
				Name: "PGPASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: nextcloudAdminSecret},
						Key:                  "dbpassword",
					},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "shared", MountPath: "/shared"},
		},
	}
}

// nextcloudDeleteContainer returns a curl Container that reads /shared/users.txt
// (written by the lookup init container), deletes each NC user via the OCS API,
// and then removes the tenant group.
func nextcloudDeleteContainer(group string) corev1.Container {
	c := nextcloudContainer("delete-group", group, nextcloudDeleteGroupScript(group))
	c.VolumeMounts = []corev1.VolumeMount{
		{Name: "shared", MountPath: "/shared"},
	}
	return c
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

// nextcloudUserLookupScript queries the Nextcloud Postgres DB for all NC user IDs
// whose LDAP DN is under the tenant OU and writes them (one per line) to /shared/users.txt.
// The LDAP DN pattern %,ou=<tenant>,% exactly matches the tenant OU without
// accidentally matching sub-tenants (e.g. ou=gtn-demo won't match ou=gtn-demo-2).
//
// If the oc_ldap_user_mapping table does not exist (Nextcloud was never fully
// initialized for this environment), the script exits 0 with an empty users.txt
// so the delete job can still proceed and remove the group.
func nextcloudUserLookupScript(tenantName string) string {
	return fmt.Sprintf(`set -eu
TABLE_EXISTS=$(psql -h "${NC_DB_HOST}" -U "${NC_DB_USER}" -d "${NC_DB_NAME}" \
  -t -A \
  -c "SELECT COUNT(*) FROM information_schema.tables WHERE table_name='oc_ldap_user_mapping'")
if [ "${TABLE_EXISTS}" = "0" ]; then
  echo "oc_ldap_user_mapping table not found (Nextcloud not yet initialized) — skipping user lookup"
  touch /shared/users.txt
else
  psql -h "${NC_DB_HOST}" -U "${NC_DB_USER}" -d "${NC_DB_NAME}" \
    -t -A \
    -c "SELECT owncloud_name FROM oc_ldap_user_mapping WHERE ldap_dn LIKE '%%,ou=%s,%%'" \
    > /shared/users.txt
fi
echo "found $(wc -l < /shared/users.txt | tr -d ' ') users to delete"`, tenantName)
}

func nextcloudDeleteGroupScript(group string) string {
	// Read user IDs written by the lookup init container and delete each user via
	// the OCS API. This clears the Nextcloud LDAP user mappings so that a re-deploy
	// does not hit a UUID conflict when the LDAP OU was purged and its users were
	// recreated with new entryUUIDs.
	return fmt.Sprintf(`set -eu
# Delete each user whose LDAP DN was under the tenant OU.
if [ -f /shared/users.txt ]; then
  while IFS= read -r USERID; do
    [ -z "${USERID}" ] && continue
    curl -sf -u "${NEXTCLOUD_ADMIN_USER}:${NEXTCLOUD_ADMIN_PASSWORD}" \
      -X DELETE \
      "${NEXTCLOUD_URL}/ocs/v1.php/cloud/users/${USERID}" \
      -H "OCS-APIRequest: true" >/dev/null 2>&1 || echo "user ${USERID} already gone"
    echo "deleted user ${USERID}"
  done < /shared/users.txt
fi
# Delete the group itself.
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
