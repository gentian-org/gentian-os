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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

func firstAdministratorWorld(t *testing.T, name string, ready bool, store *fakeRights) (*TenantReconciler, *gentianov1alpha1.Tenant, *int) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if ready {
		apimeta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{Type: conditionIdentityReady, Status: metav1.ConditionTrue, Reason: "Ready"})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).WithStatusSubresource(tenant).Build()
	asked := 0
	r := &TenantReconciler{Client: c, Scheme: scheme, KernelRealm: "kernel", KernelDomain: "example.org"}
	if store != nil {
		r.Rights = store
	}
	r.FirstAdministratorID = func(_ context.Context, realm, username string) (string, error) {
		asked++
		if realm != name || username != r.tenantAdminUsername(tenant) {
			t.Errorf("asked realm %q for %q", realm, username)
		}
		return "u-first", nil
	}
	return r, tenant, &asked
}

// The account the platform makes for a tenant is given the position every
// tenant has for administering it, once, and the tenant is marked with whom.
func TestTheFirstAdministratorIsGivenThePositionOnce(t *testing.T) {
	ctx := context.Background()
	store := newFakeRights(projected("c1", "demo")...)
	r, tenant, asked := firstAdministratorWorld(t, "demo", true, store)

	result, err := r.ensureFirstAdministrator(ctx, tenant)
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("given: %+v, %v", result, err)
	}
	holds := func() bool {
		return store.has("user:u-first", "holder", authz.Position("demo", authz.PositionTenantAdministrator))
	}
	if !holds() {
		t.Fatalf("the store after: %v", store.tuples)
	}
	live := &gentianov1alpha1.Tenant{}
	if err := r.Get(ctx, types.NamespacedName{Name: "demo"}, live); err != nil {
		t.Fatal(err)
	}
	if live.Annotations[firstAdministratorAnnotation] != "u-first" {
		t.Fatalf("the tenant is marked %q", live.Annotations[firstAdministratorAnnotation])
	}

	// The position is the tenant's to hand on: taken from the first
	// administrator, it is not given back.
	delete(store.tuples, authz.Tuple{User: "user:u-first", Relation: "holder", Object: authz.Position("demo", authz.PositionTenantAdministrator)})
	before := *asked
	if result, err := r.ensureFirstAdministrator(ctx, live); err != nil || result.RequeueAfter != 0 {
		t.Fatalf("a second pass: %+v, %v", result, err)
	}
	if holds() || *asked != before {
		t.Error("the position was given again to an administrator who had handed it on")
	}
}

// What the step needs may not be there yet: it asks again, and gives nothing
// and marks nothing until it is.
func TestTheFirstAdministratorWaitsForWhatItNeeds(t *testing.T) {
	ctx := context.Background()

	// The tenant's identity is not ready: nobody is asked for.
	store := newFakeRights(projected("c1", "demo")...)
	r, tenant, asked := firstAdministratorWorld(t, "demo", false, store)
	if result, err := r.ensureFirstAdministrator(ctx, tenant); err != nil || result.RequeueAfter == 0 || *asked != 0 {
		t.Errorf("before the identity is ready: %+v, %v, asked %d", result, err, *asked)
	}

	// The account does not exist yet.
	r, tenant, _ = firstAdministratorWorld(t, "demo", true, store)
	r.FirstAdministratorID = func(context.Context, string, string) (string, error) { return "", nil }
	if result, err := r.ensureFirstAdministrator(ctx, tenant); err != nil || result.RequeueAfter == 0 || tenant.Annotations[firstAdministratorAnnotation] != "" {
		t.Errorf("before the account exists: %+v, %v", result, err)
	}

	// The projection has not written the tenant's positions yet.
	empty := newFakeRights()
	r, tenant, _ = firstAdministratorWorld(t, "demo", true, empty)
	if result, err := r.ensureFirstAdministrator(ctx, tenant); err != nil || result.RequeueAfter == 0 || len(empty.tuples) != 0 || tenant.Annotations[firstAdministratorAnnotation] != "" {
		t.Errorf("before the projection: %+v, %v, %v", result, err, empty.tuples)
	}

	// The identity provider does not answer: an error, and nothing marked.
	r, tenant, _ = firstAdministratorWorld(t, "demo", true, store)
	r.FirstAdministratorID = func(context.Context, string, string) (string, error) { return "", errors.New("no answer") }
	if _, err := r.ensureFirstAdministrator(ctx, tenant); err == nil || tenant.Annotations[firstAdministratorAnnotation] != "" {
		t.Errorf("an identity provider that does not answer: %v", err)
	}
}

// A tenant that adopts the kernel realm has no account made for it, and a
// cluster with no rights store has nothing to give in.
func TestNoFirstAdministratorWhereThereIsNoneToGive(t *testing.T) {
	ctx := context.Background()
	store := newFakeRights(projected("c1", "platform")...)
	r, tenant, asked := firstAdministratorWorld(t, "platform", true, store)
	tenant.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
	if result, err := r.ensureFirstAdministrator(ctx, tenant); err != nil || result.RequeueAfter != 0 || *asked != 0 {
		t.Errorf("a tenant of the kernel realm: %+v, %v, asked %d", result, err, *asked)
	}
	r, tenant, asked = firstAdministratorWorld(t, "demo", true, nil)
	if result, err := r.ensureFirstAdministrator(ctx, tenant); err != nil || result.RequeueAfter != 0 || *asked != 0 {
		t.Errorf("without a rights store: %+v, %v, asked %d", result, err, *asked)
	}
}
