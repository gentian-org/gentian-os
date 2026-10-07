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
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An operator with no vault that was not told to run without one must not
// pass over the step that destroys a tenant's stored credentials: that is
// what a deployment whose vault setting was lost looks like, and it used to
// report the tenant deleted.
func TestTenantDeleteWithoutAVaultIsAnErrorUnlessDeclared(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	undeclared := &TenantReconciler{Client: c, Scheme: scheme}
	if err := undeclared.purgeTenantVault(context.Background(), tenant); !errors.Is(err, ErrNoVault) {
		t.Fatalf("no vault and no statement: err = %v, want ErrNoVault", err)
	}
	declared := &TenantReconciler{Client: c, Scheme: scheme, WithoutVault: true}
	if err := declared.purgeTenantVault(context.Background(), tenant); err != nil {
		t.Fatalf("declared to run without a vault: err = %v", err)
	}
}
