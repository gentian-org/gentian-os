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
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// The claim spells its roles one way (breakGlass) and the graph another
// (break_glass). What the projection hands the graph has to be the graph's
// spelling, or the tuple is never written: the graph refuses the whole map
// for one key it does not know, and with it the tenants and apps projected in
// the same pass.
func TestThePlatformRolesAreProjectedUnderTheGraphsRelationNames(t *testing.T) {
	claim := clusterClaim("")
	_ = unstructured.SetNestedStringMap(claim.Object, map[string]string{
		"admin":      "gentian:platform:admin",
		"breakGlass": "gentian:platform:break-glass",
	}, "spec", "platformRoles")

	r := &AuthzProjectionReconciler{
		Client:  fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(claim).Build(),
		Cluster: "test",
	}
	roles, err := r.platformRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"admin":       "gentian:platform:admin",
		"break_glass": "gentian:platform:break-glass",
	}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("platformRoles() = %v, want %v", roles, want)
	}
}

// A field the graph has no relation for is a mistake in the claim, and it
// must stay one role's mistake: the others are projected, and so are the
// tenants and apps that follow in the same Reconcile.
func TestAMisspeltPlatformRoleIsSkippedAndTheRestProjected(t *testing.T) {
	claim := clusterClaim("")
	_ = unstructured.SetNestedStringMap(claim.Object, map[string]string{
		"admin":           "gentian:platform:admin",
		"securityOfficer": "gentian:platform:security",
		"auditer":         "gentian:platform:auditor",
	}, "spec", "platformRoles")

	r := &AuthzProjectionReconciler{
		Client:  fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(claim).Build(),
		Cluster: "test",
	}
	roles, err := r.platformRoles(context.Background())
	if err != nil {
		t.Fatalf("a misspelt role must not fail the projection: %v", err)
	}
	// securityOfficer is held by name now, and a group named for it grants
	// nothing, as a misspelt field does.
	want := map[string]string{
		"admin": "gentian:platform:admin",
	}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("platformRoles() = %v, want %v", roles, want)
	}
	if !r.unknownRoleWarned["auditer"] || !r.unknownRoleWarned["securityOfficer"] {
		t.Error("the misspelt role should have been reported")
	}
	// The next pass finds nothing new to say about it.
	if _, err := r.platformRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(r.unknownRoleWarned); n != 2 {
		t.Errorf("warned about %d fields, want 2", n)
	}
}

// A role holder is keyed by the realm of the tenant the claim names -- the
// kernel realm for the platform tenant -- so the tuple answers only that
// realm's token. What a holder may not hold, a role the tenant's people may
// not hold, a scope other than the role's, a tenant the cluster does not
// have or that has no realm of its own, a subject that is no account id and
// an entry that has expired grant nothing. A perimeter approver of a tenant
// is held on that tenant; one of the platform tenant on the cluster.
func TestRoleHoldersAreProjectedByTheirTenantsRealm(t *testing.T) {
	claim := clusterClaim("")
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	holder := func(role, tenant, subject string, more ...string) interface{} {
		m := map[string]interface{}{"role": role, "tenant": tenant, "subject": subject, "username": "x@example.org"}
		for i := 0; i+1 < len(more); i += 2 {
			m[more[i]] = more[i+1]
		}
		return m
	}
	_ = unstructured.SetNestedSlice(claim.Object, []interface{}{
		holder("securityOfficer", "test", "6f1c"),
		holder("serviceAdmin", "platform", "k1", "scope", "cluster"),
		holder("perimeterApprover", "platform", "k2"),
		holder("perimeterApprover", "test", "6f1c", "scope", "tenant:test", "expiresAt", "2026-10-23T00:00:00Z"),
		holder("auditor", "test", "e1", "expiresAt", "2026-10-22T11:00:00Z"),
		holder("auditor", "test", "e2", "expiresAt", "2026-10-30T00:00:00Z"),
		holder("serviceAdmin", "test", "6f1c"),
		holder("perimeterApprover", "test", "6f1c", "scope", "cluster"),
		holder("auditor", "acme", "a1"),
		holder("admin", "test", "6f1c"),
		holder("breakGlass", "test", "6f1c"),
		holder("auditor", "gone", "b2"),
		holder("auditor", "test", "not a subject"),
	}, "spec", "platformAccess", "roleHolders")
	test := &gentianov1alpha1.Tenant{}
	test.Name = "test"
	acme := &gentianov1alpha1.Tenant{}
	acme.Name = "acme"
	acme.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "acme-people"}
	platform := &gentianov1alpha1.Tenant{}
	platform.Name = "platform"
	platform.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}

	r := &AuthzProjectionReconciler{
		Client:  fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(claim, test, acme, platform).Build(),
		Cluster: "test",
	}
	got, next, err := r.roleHolders(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []authz.RoleHolder{
		{Relation: "security_officer", Realm: "test", Subject: "6f1c"},
		{Relation: "service_admin", Realm: "kernel", Subject: "k1"},
		{Relation: "perimeter_approver", Realm: "kernel", Subject: "k2"},
		{Relation: "perimeter_approver", Realm: "test", Subject: "6f1c", Tenant: "test"},
		{Relation: "auditor", Realm: "test", Subject: "e2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("roleHolders() = %v, want %v", got, want)
	}
	if want := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next expiry %v, want %v", next, want)
	}
	rights, err := r.tenantRights(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range rights {
		if tr.Name == "test" && !reflect.DeepEqual(tr.PerimeterApprovers, []string{"user:test/6f1c"}) {
			t.Errorf("test's perimeter approvers: %v", tr.PerimeterApprovers)
		}
		if tr.Name != "test" && len(tr.PerimeterApprovers) != 0 {
			t.Errorf("%s's perimeter approvers: %v", tr.Name, tr.PerimeterApprovers)
		}
	}
}
