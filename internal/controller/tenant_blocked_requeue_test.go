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
// Preflight blocks when a requested ComponentProfile does not exist — a precondition the
// Tenant does not watch, since the profile is published by the app catalogue. With
// an empty Result nothing was scheduled, so the Tenant stayed Degraded until some
// unrelated event happened to wake it, which for a missing profile could be never.
func TestRunTenantReconcileStages_blockedRequeues(t *testing.T) {
	t.Parallel()

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Test Tenant",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "profile-that-does-not-exist"}},
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
		t.Fatal("expected the preflight stage to block on a missing ComponentProfile")
	}
	if res.RequeueAfter != tenantBlockedRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v — a blocked tenant that schedules nothing never retries",
			res.RequeueAfter, tenantBlockedRequeueAfter)
	}
	if tenant.Status.Phase != gentianov1alpha1.TenantPhaseDegraded {
		t.Errorf("phase = %q, want Degraded", tenant.Status.Phase)
	}
}
