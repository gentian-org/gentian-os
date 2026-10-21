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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The claim spells its roles one way (securityOfficer) and the graph another
// (security_officer). What the projection hands the graph has to be the
// graph's spelling, or the security officers' tuple is never written: the
// graph refuses the whole map for one key it does not know, and with it the
// tenants and apps projected in the same pass.
func TestThePlatformRolesAreProjectedUnderTheGraphsRelationNames(t *testing.T) {
	claim := clusterClaim("")
	_ = unstructured.SetNestedStringMap(claim.Object, map[string]string{
		"admin":           "gentian:platform:admin",
		"securityOfficer": "gentian:platform:security",
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
		"admin":            "gentian:platform:admin",
		"security_officer": "gentian:platform:security",
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
	want := map[string]string{
		"admin":            "gentian:platform:admin",
		"security_officer": "gentian:platform:security",
	}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("platformRoles() = %v, want %v", roles, want)
	}
	if !r.unknownRoleWarned["auditer"] {
		t.Error("the misspelt role should have been reported")
	}
	// The next pass finds nothing new to say about it.
	if _, err := r.platformRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(r.unknownRoleWarned); n != 1 {
		t.Errorf("warned about %d fields, want 1", n)
	}
}
