/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package tenancy

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func tenantNamed(name string) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{}
	t.Name = name
	return t
}

// A single-tenant cluster carries the platform tenant every install makes,
// and nothing beside it; a multi-tenant cluster is not limited here at all.
func TestASingleTenantClusterCarriesOnlyThePlatformTenant(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	platform := tenantNamed(gentianov1alpha1.SingleTenantName)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(platform).Build()

	if err := EnforceSingle(ctx, c, gentianov1alpha1.TenancyModeSingle, platform); err != nil {
		t.Fatalf("the platform tenant was refused: %v", err)
	}
	if err := EnforceSingle(ctx, c, gentianov1alpha1.TenancyModeSingle, tenantNamed("acme")); err == nil {
		t.Fatal("a second tenant was admitted")
	}
	if err := EnforceSingle(ctx, c, gentianov1alpha1.TenancyModeMulti, tenantNamed("acme")); err != nil {
		t.Fatalf("multi tenancy refused a tenant: %v", err)
	}
}
