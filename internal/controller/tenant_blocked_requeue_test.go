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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A blocked stage has to schedule its own retry.
//
// Preflight blocks when a requested AppProfile does not exist — a precondition the
// Tenant does not watch, since the profile is published by the app catalogue. With
// an empty Result nothing was scheduled, so the Tenant stayed Degraded until some
// unrelated event happened to wake it, which for a missing profile could be never.
func TestRunTenantReconcileStages_blockedRequeues(t *testing.T) {
	t.Parallel()

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantApp{{Profile: "profile-that-does-not-exist"}},
		},
	}

	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant).WithStatusSubresource(tenant).Build()
	r := &TenantReconciler{Client: c}

	state := &tenantReconcileState{tenant: tenant, start: time.Now()}
	res, err := r.runTenantReconcileStages(context.Background(), state)
	if err != nil {
		t.Fatalf("runTenantReconcileStages: %v", err)
	}
	if !state.blocked {
		t.Fatal("expected the preflight stage to block on a missing AppProfile")
	}
	if res.RequeueAfter != tenantBlockedRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v — a blocked tenant that schedules nothing never retries",
			res.RequeueAfter, tenantBlockedRequeueAfter)
	}
	if tenant.Status.Phase != gentianov1alpha1.TenantPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", tenant.Status.Phase)
	}
}
