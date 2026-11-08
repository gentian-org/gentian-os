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
	"encoding/json"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// lifecycleTenant is a tenant with one app whose OIDC client the operator's
// Job makes, so its provisioning has Jobs of every kind the loop hit: the
// realm, its administrator, its groups, and a client with a Secret.
func lifecycleTenant(t *testing.T) (*TenantReconciler, *gentianov1alpha1.Tenant) {
	t.Helper()
	s := componentDatabaseScheme(t)
	api := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Package: gentianov1alpha1.PackageSpec{API: &gentianov1alpha1.APIIntegration{}},
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Identity: &gentianov1alpha1.IdentityRequirement{OIDC: &gentianov1alpha1.OIDCClientSpec{
					ClientID: "demo-wiki", RedirectURIs: []string{"https://wiki.demo.example.test/*"},
				}},
			}},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{Apps: []gentianov1alpha1.TenantApp{{Profile: "wiki"}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(api, tenant).Build()
	return &TenantReconciler{Client: c, Scheme: s, KernelDomain: "example.test",
		Seeder: secrets.NewSeeder(memVault{}, secrets.NewDeriver("unit-test-master"))}, tenant
}

func composedProvisioningJobs(t *testing.T, r *TenantReconciler, tenant string) []batchv1.Job {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: tenantProvisioningConfigMapName(tenant), Namespace: provisioningNamespace,
	}, cm); err != nil {
		t.Fatal(err)
	}
	var jobs []batchv1.Job
	if err := json.Unmarshal([]byte(cm.Data[tenantProvisioningJobsDataKey]), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) == 0 {
		t.Fatal("the tenant's provisioning composes no Jobs")
	}
	return jobs
}

// Every Job Crossplane composes is the manifest of a provider-kubernetes
// Object, and an Object recreates what it holds the moment it is missing. A
// TTL on one of them deleted it after it ran, the Object made it again, and
// it ran again — every ten minutes, forever. None of them carries a TTL.
func TestTheComposedProvisioningJobsCarryNoTTL(t *testing.T) {
	r, tenant := lifecycleTenant(t)
	if err := r.ensureTenantProvisioningManifests(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	for _, job := range composedProvisioningJobs(t, r, tenant.Name) {
		if job.Spec.TTLSecondsAfterFinished != nil {
			t.Errorf("Job %s carries ttlSecondsAfterFinished=%d: its Object recreates it after every TTL",
				job.Name, *job.Spec.TTLSecondsAfterFinished)
		}
	}
}

// A finished Job whose script and inputs are unchanged stays where it is, so
// nothing runs again; one whose script or inputs changed is retired, so its
// Object makes it again and it runs with the new ones.
func TestAFinishedJobRunsAgainOnlyWhenWhatItRunsChanged(t *testing.T) {
	ctx := context.Background()
	r, tenant := lifecycleTenant(t)
	if err := r.ensureTenantProvisioningManifests(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	composed := composedProvisioningJobs(t, r, tenant.Name)
	unchanged, changed := composed[0].DeepCopy(), composed[1].DeepCopy()
	changed.Annotations[provisioningScriptHashAnnotation] = "an-older-script"
	for _, job := range []*batchv1.Job{unchanged, changed} {
		job.ResourceVersion = ""
		if err := r.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		job.Status.Succeeded = 1
		if err := r.Status().Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.ensureTenantProvisioningManifests(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(unchanged), &batchv1.Job{}); err != nil {
		t.Fatalf("a finished Job with nothing changed was not left alone: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(changed), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a finished Job whose script changed was not retired to run again: %v", err)
	}
}

// A Job that stays once it has run must not keep the credential it was handed:
// the Secret it owns goes when it has succeeded, and a Job still running keeps
// it.
func TestAFinishedJobsSecretGoesAndARunningJobsStays(t *testing.T) {
	ctx := context.Background()
	r, _ := lifecycleTenant(t)
	mk := func(name string, done bool) (*batchv1.Job, *corev1.Secret) {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: identityNamespace}}
		if err := r.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		if done {
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if err := r.Status().Update(ctx, job); err != nil {
				t.Fatal(err)
			}
		}
		sec := jobSecret(job, clientSecretSecretData("s3cr3t"))
		if err := r.Create(ctx, sec); err != nil {
			t.Fatal(err)
		}
		return job, sec
	}
	_, doneSecret := mk("keycloak-client-demo-done", true)
	_, runningSecret := mk("keycloak-client-demo-running", false)

	if err := r.ensureProvisioningJobSecrets(ctx, provisioningJobSecrets{
		client.ObjectKeyFromObject(doneSecret):    clientSecretSecretData("s3cr3t"),
		client.ObjectKeyFromObject(runningSecret): clientSecretSecretData("s3cr3t"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(doneSecret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a finished Job's credential stayed with it: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(runningSecret), &corev1.Secret{}); err != nil {
		t.Fatalf("a running Job's credential was taken from it: %v", err)
	}
}
