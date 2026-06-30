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

package controller_test

import (
	"context"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// nextcloudSecretTestMu serializes tests that require presence or absence of the
// shared nextcloud-admin Secret in platform-kernel.
var nextcloudSecretTestMu sync.Mutex

func testNextcloudAdminSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-admin", Namespace: "platform-kernel"},
		Data: map[string][]byte{
			"url":      []byte("http://nextcloud.platform-kernel.svc.cluster.local"),
			"username": []byte("admin"),
			"password": []byte("test-nc-password"),
		},
	}
}

func withNextcloudAdminSecret(t *testing.T) {
	t.Helper()
	nextcloudSecretTestMu.Lock()
	secret := testNextcloudAdminSecret()
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, secret); err != nil {
		if err := testClient.Create(context.Background(), secret); err != nil {
			nextcloudSecretTestMu.Unlock()
			t.Fatalf("create nextcloud-admin Secret: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = testClient.Delete(context.Background(), testNextcloudAdminSecret())
		nextcloudSecretTestMu.Unlock()
	})
}

func withoutNextcloudAdminSecret(t *testing.T) {
	t.Helper()
	nextcloudSecretTestMu.Lock()
	existing := testNextcloudAdminSecret()
	hadSecret := testClient.Get(context.Background(), types.NamespacedName{Name: existing.Name, Namespace: existing.Namespace}, existing) == nil
	if hadSecret {
		if err := testClient.Delete(context.Background(), existing); err != nil {
			nextcloudSecretTestMu.Unlock()
			t.Fatalf("delete nextcloud-admin Secret: %v", err)
		}
	}
	t.Cleanup(func() {
		if hadSecret {
			_ = testClient.Create(context.Background(), testNextcloudAdminSecret())
		}
		nextcloudSecretTestMu.Unlock()
	})
}

func newS3Profile(name string) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      name,
			DeploymentMethod: gentianov1alpha1.DeploymentMethodArgoCD,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "https://charts.example.com",
				Name:       name,
				Version:    "1.0.0",
			},
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Storage: &gentianov1alpha1.StorageRequirement{
					S3: &gentianov1alpha1.S3Requirement{BucketPerTenant: true},
				},
			},
		},
	}
}

// newWebDAVProfile creates a minimal AppProfile that requires WebDAV file access.
func newWebDAVProfile(name string) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName: name,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "https://charts.example.com",
				Name:       name,
				Version:    "1.0.0",
			},
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Storage: &gentianov1alpha1.StorageRequirement{
					Files: &gentianov1alpha1.FilesRequirement{
						Protocol: "webdav",
					},
				},
			},
		},
	}
}

// TestStorage_NoStorageApps verifies that a Tenant with no storage-requiring apps
// skips provisioning and sets StorageReady=True with reason NoStorageRequired.
func TestStorage_NoStorageApps(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "nostorage"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "No Storage Co",
			Domain:      "nostorage.example.com",
			AdminEmail:  "admin@nostorage.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, tenantReadyTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "nostorage"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	var cond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "StorageReady" {
			cond = &updated.Status.Conditions[i]
			break
		}
	}
	if cond == nil {
		t.Fatal("expected StorageReady condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected StorageReady=True, got %v", cond.Status)
	}
	if cond.Reason != "NoStorageRequired" {
		t.Errorf("expected reason NoStorageRequired, got %q", cond.Reason)
	}
}

// TestStorage_NextcloudGroupSkippedWithoutKernelSecret verifies that nc-group Jobs
// are not created when the shared Nextcloud kernel service is not deployed.
func TestStorage_NextcloudGroupSkippedWithoutKernelSecret(t *testing.T) {
	withoutNextcloudAdminSecret(t)

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "nc-always"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "NC Always Co",
			Domain:      "nc-always.example.com",
			AdminEmail:  "admin@nc-always.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	waitFor(t, tenantReadyTimeout, func() bool {
		updated := &gentianov1alpha1.Tenant{}
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "nc-always"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	job := &batchv1.Job{}
	err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "nc-group-nc-always", Namespace: "platform-kernel"}, job)
	if err == nil {
		t.Fatal("expected nc-group Job to be absent without nextcloud-admin Secret")
	}
}

// TestStorage_CreatesS3BucketJob verifies that a Tenant with an S3-requiring app
// creates the MinIO mc bucket Job in the kernel namespace with the correct labels
// and credentials.
func TestStorage_CreatesS3BucketJob(t *testing.T) {
	t.Parallel()
	profile := newS3Profile("s3-app1")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "s3create"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "S3 Create Co",
			Domain:      "s3create.example.com",
			AdminEmail:  "admin@s3create.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "s3-app1"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	job := &batchv1.Job{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "s3-bucket-s3create-s3-app1", Namespace: "platform-kernel"}, job) == nil
	})

	if job.Labels["gentianos.io/tenant"] != "s3create" {
		t.Errorf("expected tenant label 's3create', got %q", job.Labels["gentianos.io/tenant"])
	}
	if job.Labels["gentianos.io/app"] != "s3-app1" {
		t.Errorf("expected app label 's3-app1', got %q", job.Labels["gentianos.io/app"])
	}
	if len(job.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("expected at least one container in S3 bucket Job")
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "minio/mc:RELEASE.2025-04-03T17-07-56Z" {
		t.Errorf("unexpected container image %q", container.Image)
	}

	// Credentials must come from the minio-admin Secret.
	secretEnvs := make(map[string]string)
	for _, e := range container.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			secretEnvs[e.Name] = e.ValueFrom.SecretKeyRef.Name
		}
	}
	for _, required := range []string{"MINIO_ENDPOINT", "MINIO_ACCESS_KEY", "MINIO_SECRET_KEY"} {
		if secretEnvs[required] != "minio-admin" {
			t.Errorf("expected %s sourced from minio-admin Secret, got %q", required, secretEnvs[required])
		}
	}

	// BUCKET_NAME must be a non-empty literal env var.
	bucketName := ""
	for _, e := range container.Env {
		if e.Name == "BUCKET_NAME" {
			bucketName = e.Value
		}
	}
	if bucketName == "" {
		t.Error("expected BUCKET_NAME env var to be set")
	}
}

// TestStorage_CreatesNextcloudGroupJob verifies that a Tenant with a WebDAV-requiring
// app creates the Nextcloud group Job in the kernel namespace.
func TestStorage_CreatesNextcloudGroupJob(t *testing.T) {
	withNextcloudAdminSecret(t)

	profile := newWebDAVProfile("webdav-app1")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "nccreate"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "NC Create Co",
			Domain:      "nccreate.example.com",
			AdminEmail:  "admin@nccreate.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "webdav-app1"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	job := &batchv1.Job{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "nc-group-nccreate", Namespace: "platform-kernel"}, job) == nil
	})

	if job.Labels["gentianos.io/tenant"] != "nccreate" {
		t.Errorf("expected tenant label 'nccreate', got %q", job.Labels["gentianos.io/tenant"])
	}
	if len(job.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("expected at least one container in Nextcloud group Job")
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "curlimages/curl:8.7.1" {
		t.Errorf("unexpected container image %q", container.Image)
	}

	// Credentials must come from the nextcloud-admin Secret.
	secretEnvs := make(map[string]string)
	for _, e := range container.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			secretEnvs[e.Name] = e.ValueFrom.SecretKeyRef.Name
		}
	}
	for _, required := range []string{"NEXTCLOUD_URL", "NEXTCLOUD_ADMIN_USER", "NEXTCLOUD_ADMIN_PASSWORD"} {
		if secretEnvs[required] != "nextcloud-admin" {
			t.Errorf("expected %s sourced from nextcloud-admin Secret, got %q", required, secretEnvs[required])
		}
	}
}

// TestStorage_SetsReadyWhenAllJobsDone verifies that StorageReady=True and
// Phase=Ready are set only after all storage Jobs have completed.
func TestStorage_SetsReadyWhenAllJobsDone(t *testing.T) {
	t.Parallel()
	profile := newS3Profile("s3-app2")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "storageready"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Storage Ready Co",
			Domain:      "storageready.example.com",
			AdminEmail:  "admin@storageready.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "s3-app2"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Phase should be Provisioning while the S3 Job is pending.
	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, jobAppearTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "storageready"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseProvisioning
	})

	// Wait for the bucket Job then mark it complete.
	waitFor(t, jobAppearTimeout, func() bool {
		job := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "s3-bucket-storageready-s3-app2", Namespace: "platform-kernel"}, job) == nil
	})
	markJobComplete(t, "s3-bucket-storageready-s3-app2", "platform-kernel")

	// Phase=Ready and StorageReady=True should follow.
	waitFor(t, tenantReadyTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "storageready"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	var cond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "StorageReady" {
			cond = &updated.Status.Conditions[i]
			break
		}
	}
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("expected StorageReady=True, got %v", cond)
	}
}

// TestStorage_DeleteDeletePolicy_CreatesDeleteJobs verifies that both the S3 delete
// Job and Nextcloud delete Job are created on DeletionPolicy=Delete.
func TestStorage_DeleteDeletePolicy_CreatesDeleteJobs(t *testing.T) {
	withNextcloudAdminSecret(t)

	s3Prof := newS3Profile("s3-app3")
	if err := testClient.Create(context.Background(), s3Prof); err != nil {
		t.Fatalf("create S3 AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), s3Prof) })

	webdavProf := newWebDAVProfile("webdav-app2")
	if err := testClient.Create(context.Background(), webdavProf); err != nil {
		t.Fatalf("create WebDAV AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), webdavProf) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "storagedelete"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Storage Delete Co",
			Domain:         "storagedelete.example.com",
			AdminEmail:     "admin@storagedelete.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "s3-app3"},
				{Profile: "webdav-app2"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for setup Jobs to be created to confirm storage reconciler ran.
	waitFor(t, jobAppearTimeout, func() bool {
		job := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "s3-bucket-storagedelete-s3-app3", Namespace: "platform-kernel"}, job) == nil
	})

	// Delete the tenant.
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	// deleteIdentity and deleteLDAP run before deleteStorage; mark their jobs.
	go markJobCompleteWhenReady("keycloak-realm-delete-storagedelete", "platform-kernel")

	// S3 delete Job should appear.
	s3DeleteJob := &batchv1.Job{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "s3-delete-storagedelete-s3-app3", Namespace: "platform-kernel"}, s3DeleteJob) == nil
	})
	if s3DeleteJob.Labels["gentianos.io/tenant"] != "storagedelete" {
		t.Errorf("expected tenant label 'storagedelete', got %q", s3DeleteJob.Labels["gentianos.io/tenant"])
	}

	// Nextcloud delete Job should appear.
	ncDeleteJob := &batchv1.Job{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "nc-group-delete-storagedelete", Namespace: "platform-kernel"}, ncDeleteJob) == nil
	})
	if ncDeleteJob.Labels["gentianos.io/tenant"] != "storagedelete" {
		t.Errorf("expected tenant label 'storagedelete' on NC delete job, got %q", ncDeleteJob.Labels["gentianos.io/tenant"])
	}
}
