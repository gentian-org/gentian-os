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
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// The app's key pair is handed to the Job that makes its identity in a
// Secret the Job owns, and is in nothing else the operator writes: not in
// the Job's spec, not in the provisioning ConfigMap Crossplane composes the
// Job from.
func TestTheBucketJobReadsTheAppsKeyFromASecretItOwns(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEAWEEDFS_IMAGE", "")
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Storage: &gentianov1alpha1.StorageRequirement{S3: &gentianov1alpha1.S3Requirement{BucketPerTenant: true}},
			}},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{Apps: []gentianov1alpha1.TenantApp{{Profile: "wiki"}}},
	}
	s := componentDatabaseScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(profile, tenant).Build()
	vault := memVault{}
	r := &TenantReconciler{Client: c, Scheme: s, Seeder: secrets.NewSeeder(vault, secrets.NewDeriver("unit-test-master"))}

	jobs, keys, err := r.buildDataPlaneJobs(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	creds := vault["gentian-os/tenants/demo/apps/wiki/s3"]
	if creds["access-key"] == "" || creds["secret-key"] == "" {
		t.Fatalf("no key pair was seeded for the app: %v", vault)
	}
	payload, err := serializeProvisioningJobs(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, creds["access-key"]) || strings.Contains(payload, creds["secret-key"]) {
		t.Fatal("the provisioning manifests carry the app's key pair")
	}
	name := s3BucketJobName("demo", "wiki")
	var bucketJob *batchv1.Job
	for i := range jobs {
		if jobs[i].Name == name {
			bucketJob = &jobs[i]
		}
	}
	if bucketJob == nil {
		t.Fatalf("no bucket Job among %d", len(jobs))
	}
	refs := map[string]string{}
	for _, e := range bucketJob.Spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			refs[e.Name] = e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
		}
	}
	if refs["APP_ACCESS_KEY"] != name+"/accessKey" || refs["APP_SECRET_KEY"] != name+"/secretKey" {
		t.Errorf("the bucket Job does not read the key pair from its own Secret: %v", refs)
	}

	// Before Crossplane has made the Job there is nothing to own the Secret.
	if err := r.ensureBucketKeySecrets(ctx, keys); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	key := types.NamespacedName{Name: name, Namespace: s3Namespace}
	if err := c.Get(ctx, key, sec); !apierrors.IsNotFound(err) {
		t.Fatalf("a Secret was made before its Job (%v)", err)
	}

	// The Job as Crossplane makes it.
	made := bucketJob.DeepCopy()
	made.UID = "job-uid-1"
	if err := c.Create(ctx, made); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureBucketKeySecrets(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, sec); err != nil {
		t.Fatalf("no Secret beside the Job: %v", err)
	}
	if string(sec.Data["accessKey"]) != creds["access-key"] || string(sec.Data["secretKey"]) != creds["secret-key"] {
		t.Error("the Secret holds another key pair than the vault's")
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" ||
		sec.OwnerReferences[0].Name != name || sec.OwnerReferences[0].UID != "job-uid-1" {
		t.Errorf("the Secret is not owned by its Job: %+v", sec.OwnerReferences)
	}

	// A Job made again (a retired one's Secret went with it) owns it anew.
	if err := c.Delete(ctx, made); err != nil {
		t.Fatal(err)
	}
	again := bucketJob.DeepCopy()
	again.UID = "job-uid-2"
	if err := c.Create(ctx, again); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureBucketKeySecrets(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, sec); err != nil || sec.OwnerReferences[0].UID != "job-uid-2" {
		t.Errorf("the Secret is not owned by the Job made again (%v): %+v", err, sec.OwnerReferences)
	}

	// A finished Job is handed nothing.
	if err := c.Delete(ctx, sec); err != nil {
		t.Fatal(err)
	}
	again.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, again); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureBucketKeySecrets(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, sec); !apierrors.IsNotFound(err) {
		t.Errorf("a finished Job was handed a Secret (%v)", err)
	}
}

// A bucket's restore runs the same provisioning step, and its key pair
// reaches it the same way: a Secret of the restore Job's name, owned by it.
func TestABucketRestoreReadsTheAppsKeyFromASecretItOwns(t *testing.T) {
	ctx := context.Background()
	s := componentDatabaseScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &TenantRestoreReconciler{Client: c, Scheme: s}
	restore := &gentianov1alpha1.TenantRestore{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "tenant-demo"}}
	job := backup.S3RestoreJob(backup.JobParams{Name: "trs-demo-r1-wiki-s3r", Namespace: s3Namespace, Tenant: "demo", App: "wiki", Export: "r1"},
		backup.Decryption{}, "apps/wiki/bucket.tar.gz.age", "demo-wiki",
		backup.ObjectStorageProvisionContainer("provision-bucket", "demo-wiki", "trs-demo-r1-wiki-s3r"))
	for _, ic := range job.Spec.Template.Spec.InitContainers {
		for _, e := range ic.Env {
			if strings.Contains(e.Value, "the-access-key") || strings.Contains(e.Value, "the-secret-key") {
				t.Fatalf("the restore Job carries the key in %s", e.Name)
			}
		}
	}
	unit := captureUnit{JobName: job.Name, Job: job, Key: &objectStoreKey{accessKey: "the-access-key", secretKey: "the-secret-key"}}
	// First pass makes the Job; the Secret follows it, at the latest on the
	// next pass, owned by the Job as the cluster has it.
	for i := 0; i < 2; i++ {
		if _, err := r.ensureRestoreJob(ctx, restore, unit); err != nil {
			t.Fatal(err)
		}
	}
	made := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), made); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: s3Namespace}, sec); err != nil {
		t.Fatalf("no Secret beside the restore Job: %v", err)
	}
	if string(sec.Data["accessKey"]) != "the-access-key" || string(sec.Data["secretKey"]) != "the-secret-key" {
		t.Error("the Secret holds another key pair")
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != job.Name || sec.OwnerReferences[0].UID != made.UID {
		t.Errorf("the Secret is not owned by the restore Job: %+v", sec.OwnerReferences)
	}
}
