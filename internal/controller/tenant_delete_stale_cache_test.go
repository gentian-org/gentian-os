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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// staleCacheScheme carries the types the deletion path touches before it would
// reach anything cluster-specific.
func staleCacheScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		gentianov1alpha1.AddToScheme,
		batchv1.AddToScheme,
		corev1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build scheme: %v", err)
		}
	}
	return s
}

// deletingTenant is a Tenant mid-deletion: finalizer still attached, deletion
// timestamp set. This is what the cache keeps serving for a moment after the
// finalizer comes off and the object is really gone.
func deletingTenant() *gentianov1alpha1.Tenant {
	now := metav1.NewTime(time.Now().Add(-time.Second))
	return &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "stalecache",
			DeletionTimestamp: &now,
			Finalizers:        []string{tenantFinalizer},
		},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Stale Cache Co",
			Domain:         "stalecache.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps:           []gentianov1alpha1.TenantApp{{Profile: "stale-app"}},
		},
	}
}

// TestReconcile_TenantGoneFromAPI_DoesNotResurrectCleanupJobs covers the window
// after purgeTenantKernelResources has run and the finalizer has been removed.
//
// The Tenant is gone from the API server but the cache still serves it, and a
// reconcile queued before the removal still arrives. Running the deletion chain
// then re-creates the cleanup Job whose purge just completed, and nothing is
// left to complete or purge it a second time — the Tenant that would have
// driven it no longer exists, so the Job stays on the cluster.
func TestReconcile_TenantGoneFromAPI_DoesNotResurrectCleanupJobs(t *testing.T) {
	t.Parallel()
	scheme := staleCacheScheme(t)

	// The cache still has the Tenant; the API server does not.
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deletingTenant()).Build()
	apiReader := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &TenantReconciler{
		Client:       cached,
		APIReader:    apiReader,
		Scheme:       scheme,
		KernelDomain: "platform.example.test",
		KernelRealm:  "kernel",
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "stalecache"},
	})
	if err != nil {
		t.Fatalf("reconcile of an already-deleted Tenant should be a no-op, got error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue for a Tenant that no longer exists, got RequeueAfter=%s", res.RequeueAfter)
	}

	jobs := &batchv1.JobList{}
	if err := cached.List(context.Background(), jobs, client.InNamespace(kernelNamespace)); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		names := make([]string, 0, len(jobs.Items))
		for i := range jobs.Items {
			names = append(names, jobs.Items[i].Name)
		}
		t.Errorf("deletion chain ran for a Tenant the API server no longer has and left %d Job(s) behind: %v",
			len(jobs.Items), names)
	}
}

// TestTenantGoneFromAPI covers the guard itself, because it decides whether the
// deletion chain runs at all: answering "gone" for a live Tenant would strand
// its cleanup.
func TestTenantGoneFromAPI(t *testing.T) {
	t.Parallel()
	scheme := staleCacheScheme(t)
	name := types.NamespacedName{Name: "stalecache"}

	t.Run("gone when the API server does not have it", func(t *testing.T) {
		r := &TenantReconciler{APIReader: fake.NewClientBuilder().WithScheme(scheme).Build()}
		if !r.tenantGoneFromAPI(context.Background(), name) {
			t.Error("expected gone for a Tenant absent from the API server")
		}
	})

	t.Run("present when the API server still has it", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deletingTenant()).Build()
		r := &TenantReconciler{APIReader: reader}
		if r.tenantGoneFromAPI(context.Background(), name) {
			t.Error("a Tenant still on the API server must not be treated as gone")
		}
	})

	t.Run("present when no uncached reader is configured", func(t *testing.T) {
		r := &TenantReconciler{}
		if r.tenantGoneFromAPI(context.Background(), name) {
			t.Error("without an APIReader the guard must not suppress the deletion chain")
		}
	})
}
