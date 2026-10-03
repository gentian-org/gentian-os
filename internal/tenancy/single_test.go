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
