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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var argocdAppGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Application",
}

// newRedisProfile creates a minimal AppProfile that requires a Redis cache.
func newRedisProfile(name string) *gentianov1alpha1.AppProfile {
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
				Cache: &gentianov1alpha1.CacheRequirement{
					Engine: gentianov1alpha1.CacheEngineRedis,
				},
			},
		},
	}
}

// newMemcachedProfile creates a minimal AppProfile that requires a Memcached cache.
func newMemcachedProfile(name string) *gentianov1alpha1.AppProfile {
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
				Cache: &gentianov1alpha1.CacheRequirement{
					Engine: gentianov1alpha1.CacheEngineMemcached,
				},
			},
		},
	}
}

// TestCache_NoCacheApps verifies that a Tenant with no cache-requiring apps
// skips provisioning and sets CacheReady=True with reason NoCacheRequired.
func TestCache_NoCacheApps(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "nocache"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "No Cache Co",
			Domain:      "nocache.example.com",
			AdminEmail:  "admin@nocache.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, tenantReadyTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "nocache"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	var cond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "CacheReady" {
			cond = &updated.Status.Conditions[i]
			break
		}
	}
	if cond == nil {
		t.Fatal("expected CacheReady condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected CacheReady=True, got %v", cond.Status)
	}
	if cond.Reason != "NoCacheRequired" {
		t.Errorf("expected reason NoCacheRequired, got %q", cond.Reason)
	}
}

// TestCache_CreatesRedisACLJob verifies that a Tenant with a Redis-requiring app
// creates the redis-cli ACL SETUSER Job in the kernel namespace with correct labels.
func TestCache_CreatesRedisACLJob(t *testing.T) {
	t.Parallel()
	profile := newRedisProfile("redis-app1")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "rediscreate"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Redis Create Co",
			Domain:      "rediscreate.example.com",
			AdminEmail:  "admin@rediscreate.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "redis-app1"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	job := &batchv1.Job{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "redis-acl-rediscreate-redis-app1", Namespace: "platform-kernel"}, job) == nil
	})

	if job.Labels["gentianos.io/tenant"] != "rediscreate" {
		t.Errorf("expected tenant label 'rediscreate', got %q", job.Labels["gentianos.io/tenant"])
	}
	if job.Labels["gentianos.io/app"] != "redis-app1" {
		t.Errorf("expected app label 'redis-app1', got %q", job.Labels["gentianos.io/app"])
	}
	if len(job.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("expected at least one container in Redis ACL Job")
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "redis:7-alpine" {
		t.Errorf("unexpected container image %q", container.Image)
	}

	// Credentials must come from the redis-admin Secret.
	secretEnvs := make(map[string]string)
	for _, e := range container.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			secretEnvs[e.Name] = e.ValueFrom.SecretKeyRef.Name
		}
	}
	for _, required := range []string{"REDIS_HOST", "REDIS_PASSWORD"} {
		if secretEnvs[required] != "redis-admin" {
			t.Errorf("expected %s sourced from redis-admin Secret, got %q", required, secretEnvs[required])
		}
	}
}

// TestCache_CreatesMemcachedApplication verifies that a Tenant with a Memcached-requiring
// app creates the ArgoCD Application CR in the argocd namespace.
func TestCache_CreatesMemcachedApplication(t *testing.T) {
	t.Parallel()
	profile := newMemcachedProfile("mc-app1")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "mccreate"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "MC Create Co",
			Domain:      "mccreate.example.com",
			AdminEmail:  "admin@mccreate.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "mc-app1"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argocdAppGVK)
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "memcached-mccreate", Namespace: "argocd"}, app) == nil
	})

	if app.GetLabels()["gentianos.io/tenant"] != "mccreate" {
		t.Errorf("expected tenant label 'mccreate', got %q", app.GetLabels()["gentianos.io/tenant"])
	}

	// Verify destination namespace points to the tenant namespace.
	destNS, _, _ := unstructured.NestedString(app.Object, "spec", "destination", "namespace")
	if destNS != "tenant-mccreate" {
		t.Errorf("expected destination namespace 'tenant-mccreate', got %q", destNS)
	}

	// Verify chart reference is Bitnami Memcached.
	chart, _, _ := unstructured.NestedString(app.Object, "spec", "source", "chart")
	if chart != "memcached" {
		t.Errorf("expected chart 'memcached', got %q", chart)
	}
}

// TestCache_SetsReadyWhenRedisJobsDone verifies that CacheReady=True and Phase=Ready
// follow once all Redis ACL Jobs have completed.
func TestCache_SetsReadyWhenRedisJobsDone(t *testing.T) {
	t.Parallel()
	profile := newRedisProfile("redis-app2")
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "cacheready"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Cache Ready Co",
			Domain:      "cacheready.example.com",
			AdminEmail:  "admin@cacheready.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "redis-app2"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Phase should be Provisioning while the Redis ACL Job is pending.
	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 10*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "cacheready"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseProvisioning
	})

	// Wait for the ACL Job, then mark it complete.
	waitFor(t, 10*time.Second, func() bool {
		job := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "redis-acl-cacheready-redis-app2", Namespace: "platform-kernel"}, job) == nil
	})
	markJobComplete(t, "redis-acl-cacheready-redis-app2", "platform-kernel")

	// Phase=Ready and CacheReady=True should follow.
	waitFor(t, 15*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "cacheready"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	var cond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "CacheReady" {
			cond = &updated.Status.Conditions[i]
			break
		}
	}
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("expected CacheReady=True, got %v", cond)
	}
}

// TestCache_DeleteDeletePolicy_CreatesDeleteJobsAndDeletesApplication verifies
// that on DeletionPolicy=Delete the Redis delete Job is created and the Memcached
// Application CR is removed.
func TestCache_DeleteDeletePolicy_CreatesDeleteJobsAndDeletesApplication(t *testing.T) {
	t.Parallel()
	redisProf := newRedisProfile("redis-app3")
	if err := testClient.Create(context.Background(), redisProf); err != nil {
		t.Fatalf("create Redis AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), redisProf) })

	mcProf := newMemcachedProfile("mc-app2")
	if err := testClient.Create(context.Background(), mcProf); err != nil {
		t.Fatalf("create Memcached AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), mcProf) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "cachedelete"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Cache Delete Co",
			Domain:         "cachedelete.example.com",
			AdminEmail:     "admin@cachedelete.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "redis-app3"},
				{Profile: "mc-app2"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for setup resources to confirm the cache reconciler ran.
	waitFor(t, 10*time.Second, func() bool {
		job := &batchv1.Job{}
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "redis-acl-cachedelete-redis-app3", Namespace: "platform-kernel"}, job) == nil
	})
	waitFor(t, 10*time.Second, func() bool {
		app := &unstructured.Unstructured{}
		app.SetGroupVersionKind(argocdAppGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "memcached-cachedelete", Namespace: "argocd"}, app) == nil
	})

	// Delete the tenant.
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	// deleteIdentity and deleteLDAP run before deleteCache; mark their jobs.
	go markJobCompleteWhenReady("keycloak-realm-delete-cachedelete", "platform-kernel")
	go markJobCompleteWhenReady("ldap-ou-delete-cachedelete", "platform-kernel")
	go markJobCompleteWhenReady("nc-group-delete-cachedelete", "platform-kernel")

	// Redis delete Job should appear.
	deleteJob := &batchv1.Job{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "redis-acl-delete-cachedelete-redis-app3", Namespace: "platform-kernel"}, deleteJob) == nil
	})
	if deleteJob.Labels["gentianos.io/tenant"] != "cachedelete" {
		t.Errorf("expected tenant label 'cachedelete', got %q", deleteJob.Labels["gentianos.io/tenant"])
	}

	// Memcached Application CR should be gone.
	waitFor(t, 10*time.Second, func() bool {
		app := &unstructured.Unstructured{}
		app.SetGroupVersionKind(argocdAppGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "memcached-cachedelete", Namespace: "argocd"}, app)
		return err != nil // NotFound = success
	})
}
