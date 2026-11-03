/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// newComponentProfile builds a ComponentProfile with the crossplane DeploymentMethod and
// optional ValueMapping.
func newComponentProfile(name string, vm *gentianov1alpha1.ValueMapping) *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Package: gentianov1alpha1.PackageSpec{Chart: &gentianov1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       name,
				Version:    "1.2.3",
			}, ValueMapping: vm},
		},
	}
}

// TestApps_NoApps verifies that a Tenant with no apps skips provisioning and
// sets AppsReady=True with reason NoAppsConfigured.
func TestApps_NoApps(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "noapps"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "No Apps Co",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, tenantReadyTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "noapps"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	var cond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "AppsReady" {
			cond = &updated.Status.Conditions[i]
			break
		}
	}
	if cond == nil {
		t.Fatal("expected AppsReady condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected AppsReady=True, got %v", cond.Status)
	}
	if cond.Reason != "NoAppsConfigured" {
		t.Errorf("expected reason NoAppsConfigured, got %q", cond.Reason)
	}
}

// TestApps_CleanupOrphanedAppWorkload verifies that Jobs and orphan Job pods for
// apps removed from spec.apps are deleted during app reconciliation.
func TestApps_CleanupOrphanedAppWorkload(t *testing.T) {
	t.Parallel()
	profile := newComponentProfile("orphan-wl-app", nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create ComponentProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-app-wl"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Orphan App Workload 2",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "orphan-wl-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	nsName := "tenant-orphan-app-wl"
	waitFor(t, tenantReadyTimeout, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: nsName}, &corev1.Namespace{}) == nil
	})

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "catalogue-app-oidc-seed",
			Namespace: nsName,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gentian-os",
				"gentianos.io/tenant":          tenant.Name,
				"gentianos.io/app":             "catalogue-app",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "seed",
						Image: "pause:3.9",
					}},
				},
			},
		},
	}
	if err := testClient.Create(context.Background(), job); err != nil {
		t.Fatalf("create orphaned app Job: %v", err)
	}

	controller := true
	orphanPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "catalogue-app-oidc-seed-orphan",
			Namespace: nsName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       "catalogue-app-oidc-seed",
				UID:        "00000000-0000-0000-0000-000000000001",
				Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "seed",
				Image: "pause:3.9",
			}},
		},
	}
	if err := testClient.Create(context.Background(), orphanPod); err != nil {
		t.Fatalf("create orphaned Job pod: %v", err)
	}

	for {
		updated := &gentianov1alpha1.Tenant{}
		if err := testClient.Get(context.Background(), types.NamespacedName{Name: tenant.Name}, updated); err != nil {
			t.Fatalf("get tenant: %v", err)
		}
		updated.Spec.DisplayName = "Orphan App Workload Updated"
		if err := testClient.Update(context.Background(), updated); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			t.Fatalf("update tenant: %v", err)
		}
		break
	}

	waitFor(t, tenantReadyTimeout, func() bool {
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "catalogue-app-oidc-seed", Namespace: nsName}, &batchv1.Job{})
		return apierrors.IsNotFound(err)
	})

	waitFor(t, tenantReadyTimeout, func() bool {
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "catalogue-app-oidc-seed-orphan", Namespace: nsName}, &corev1.Pod{})
		return apierrors.IsNotFound(err)
	})
}

// TestApps_CleanupOrphanedAppWorkloadOwnerlessJobPod verifies that Job pods
// without ownerReferences are removed when the owning Job is already gone.
func TestApps_CleanupOrphanedAppWorkloadOwnerlessJobPod(t *testing.T) {
	t.Parallel()
	profile := newComponentProfile("orphan-wl-app2", nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create ComponentProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-app-wl2"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Test Tenant",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "orphan-wl-app2"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	nsName := "tenant-orphan-app-wl2"
	waitFor(t, tenantReadyTimeout, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: nsName}, &corev1.Namespace{}) == nil
	})

	ownerlessPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "consumer-app-connector-register-orphan",
			Namespace: nsName,
			Labels: map[string]string{
				"batch.kubernetes.io/job-name": "consumer-app-connector-register",
				"job-name":                     "consumer-app-connector-register",
				"gentianos.io/app":             "consumer-app",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "register",
				Image: "pause:3.9",
			}},
		},
	}
	if err := testClient.Create(context.Background(), ownerlessPod); err != nil {
		t.Fatalf("create ownerless Job pod: %v", err)
	}

	for {
		updated := &gentianov1alpha1.Tenant{}
		if err := testClient.Get(context.Background(), types.NamespacedName{Name: tenant.Name}, updated); err != nil {
			t.Fatalf("get tenant: %v", err)
		}
		updated.Spec.DisplayName = "Orphan App Workload 2 Updated"
		if err := testClient.Update(context.Background(), updated); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			t.Fatalf("update tenant: %v", err)
		}
		break
	}

	waitFor(t, tenantReadyTimeout, func() bool {
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "consumer-app-connector-register-orphan", Namespace: nsName}, &corev1.Pod{})
		return apierrors.IsNotFound(err)
	})
}
