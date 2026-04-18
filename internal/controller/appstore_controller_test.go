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
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/webhook"
)

// --- helpers ---

func makeProfile(name string) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      name,
			DeploymentMethod: gentianov1alpha1.DeploymentMethodArgoCD,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       name,
				Version:    "1.0.0",
			},
		},
	}
}

func makeProfileWithKernel(name string, kr *gentianov1alpha1.KernelRequirements) *gentianov1alpha1.AppProfile {
	p := makeProfile(name)
	p.Spec.KernelRequirements = kr
	return p
}

// waitForCatalogueApps polls the AppCatalogue "default" until it lists at
// least minCount apps or the deadline is reached.
func waitForCatalogueApps(t *testing.T, minCount int) *gentianov1alpha1.AppCatalogue {
	t.Helper()
	ctx := context.Background()
	var cat gentianov1alpha1.AppCatalogue
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := testClient.Get(ctx, types.NamespacedName{Name: "default"}, &cat); err != nil {
			return false, nil
		}
		return cat.Status.TotalApps >= minCount, nil
	})
	if err != nil {
		t.Fatalf("AppCatalogue did not reach %d apps within timeout: %v", minCount, err)
	}
	return &cat
}

// --- tests ---

// TestAppCatalogue_ListsAllProfiles: create 5 AppProfiles → catalogue lists all 5.
func TestAppCatalogue_ListsAllProfiles(t *testing.T) {
	ctx := context.Background()

	names := []string{"cat-alpha", "cat-beta", "cat-gamma", "cat-delta", "cat-epsilon"}
	for _, n := range names {
		p := makeProfile(n)
		if err := testClient.Create(ctx, p); err != nil {
			t.Fatalf("create AppProfile %q: %v", n, err)
		}
		t.Cleanup(func() { _ = testClient.Delete(ctx, p) })
	}

	cat := waitForCatalogueApps(t, 5)
	if cat.Status.TotalApps < 5 {
		t.Fatalf("expected TotalApps >= 5, got %d", cat.Status.TotalApps)
	}

	// Verify all names appear in the catalogue.
	inCat := make(map[string]bool)
	for _, e := range cat.Status.Apps {
		inCat[e.Name] = true
	}
	for _, n := range names {
		if !inCat[n] {
			t.Errorf("AppProfile %q not in catalogue", n)
		}
	}
}

// TestAppCatalogue_KernelRequirementLabels: verify labels are derived correctly.
func TestAppCatalogue_KernelRequirementLabels(t *testing.T) {
	ctx := context.Background()

	p := makeProfileWithKernel("cat-with-kernel", &gentianov1alpha1.KernelRequirements{
		Identity: &gentianov1alpha1.IdentityRequirement{
			OIDC: true,
			LDAP: &gentianov1alpha1.LDAPRequirement{Sync: true},
		},
		Database: &gentianov1alpha1.DatabaseRequirement{
			Engine:            gentianov1alpha1.DatabaseEnginePostgreSQL,
			DatabasePerTenant: true,
		},
		Storage: &gentianov1alpha1.StorageRequirement{
			S3: &gentianov1alpha1.S3Requirement{BucketPerTenant: true},
		},
	})
	if err := testClient.Create(ctx, p); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, p) })

	// Poll until the entry appears.
	var entry *gentianov1alpha1.CatalogueEntry
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		var cat gentianov1alpha1.AppCatalogue
		if err := testClient.Get(ctx, types.NamespacedName{Name: "default"}, &cat); err != nil {
			return false, nil
		}
		for i := range cat.Status.Apps {
			if cat.Status.Apps[i].Name == "cat-with-kernel" {
				entry = &cat.Status.Apps[i]
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil || entry == nil {
		t.Fatalf("AppProfile cat-with-kernel not found in catalogue: %v", err)
	}

	wantLabels := map[string]bool{"oidc": true, "ldap": true, "postgresql": true, "s3": true}
	for _, l := range entry.KernelRequirements {
		if !wantLabels[l] {
			t.Errorf("unexpected kernel requirement label %q", l)
		}
		delete(wantLabels, l)
	}
	for missing := range wantLabels {
		t.Errorf("missing kernel requirement label %q", missing)
	}
}

// TestAppCatalogue_InstalledCountUpdatesOnTenantChange: install + uninstall cycle.
func TestAppCatalogue_InstalledCountUpdatesOnTenantChange(t *testing.T) {
	ctx := context.Background()

	profile := makeProfile("cat-installable")
	if err := testClient.Create(ctx, profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, profile) })

	// Create a tenant referencing the profile.
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "cat-tenant-install"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Catalogue Install Tenant",
			Domain:      "cat-install.example.com",
			AdminEmail:  "admin@cat-install.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "cat-installable"}},
		},
	}
	if err := testClient.Create(ctx, tenant); err != nil {
		t.Fatalf("create Tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, tenant) })

	// Wait until InstalledCount == 1.
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		var cat gentianov1alpha1.AppCatalogue
		if err := testClient.Get(ctx, types.NamespacedName{Name: "default"}, &cat); err != nil {
			return false, nil
		}
		for _, e := range cat.Status.Apps {
			if e.Name == "cat-installable" {
				return e.InstalledCount == 1, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("InstalledCount did not reach 1: %v", err)
	}

	// Re-fetch before update to get the latest resourceVersion (avoid conflict).
	if err := testClient.Get(ctx, types.NamespacedName{Name: tenant.Name}, tenant); err != nil {
		t.Fatalf("re-fetch tenant: %v", err)
	}
	// Remove all apps from the tenant (simulate uninstall).
	tenant.Spec.Apps = nil
	if err := testClient.Update(ctx, tenant); err != nil {
		t.Fatalf("update tenant: %v", err)
	}

	// Wait until InstalledCount == 0.
	err = wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		var cat gentianov1alpha1.AppCatalogue
		if err := testClient.Get(ctx, types.NamespacedName{Name: "default"}, &cat); err != nil {
			return false, nil
		}
		for _, e := range cat.Status.Apps {
			if e.Name == "cat-installable" {
				return e.InstalledCount == 0, nil
			}
		}
		return true, nil // entry vanished entirely — also fine
	})
	if err != nil {
		t.Fatalf("InstalledCount did not drop to 0: %v", err)
	}
}

// TestTenantValidator_MaxAppsExceeded: adding beyond maxApps is rejected by the validator logic.
func TestTenantValidator_MaxAppsExceeded(t *testing.T) {
	ctx := context.Background()

	// Create 3 AppProfiles.
	for _, n := range []string{"tval-a", "tval-b", "tval-c"} {
		p := makeProfile(n)
		if err := testClient.Create(ctx, p); err != nil {
			t.Fatalf("create AppProfile %q: %v", n, err)
		}
		t.Cleanup(func(name string) func() {
			return func() {
				_ = testClient.Delete(ctx, &gentianov1alpha1.AppProfile{ObjectMeta: metav1.ObjectMeta{Name: name}})
			}
		}(n))
	}

	maxApps := int32(2)
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("tval-maxapps-%d", time.Now().UnixNano())},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Validator Max Test",
			Domain:      "tval.example.com",
			AdminEmail:  "admin@tval.example.com",
			Quotas:      &gentianov1alpha1.TenantQuotas{MaxApps: maxApps},
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "tval-a"},
				{Profile: "tval-b"},
				{Profile: "tval-c"}, // one over the quota
			},
		},
	}

	// Run the real webhook validation logic.
	v := &webhook.TenantValidator{Client: testClient}
	if err := v.Validate(ctx, tenant); err == nil {
		t.Error("expected validation error for app count exceeding maxApps, got nil")
	}
}

// TestTenantValidator_MissingAppProfile: adding a tenant with a non-existent AppProfile.
func TestTenantValidator_MissingAppProfile(t *testing.T) {
	ctx := context.Background()

	// Confirm the AppProfile does not exist.
	missing := "tval-nonexistent-profile"
	var ap gentianov1alpha1.AppProfile
	if err := testClient.Get(ctx, types.NamespacedName{Name: missing}, &ap); err == nil {
		t.Skipf("AppProfile %q unexpectedly exists — skipping", missing)
	}

	// A Tenant referencing a nonexistent AppProfile should still be created (no live
	// webhook in envtest without TLS), but we verify validator logic returns an error.
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "tval-missing-profile"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Missing Profile Tenant",
			Domain:      "missing.example.com",
			AdminEmail:  "admin@missing.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: missing}},
		},
	}

	v := &webhook.TenantValidator{Client: testClient}
	if err := v.Validate(ctx, tenant); err == nil {
		t.Error("expected validation error for nonexistent AppProfile, got nil")
	}
}

// TestAppCatalogue_MultipleTenantsInstalledCount: two tenants, same app → count == 2.
func TestAppCatalogue_MultipleTenantsInstalledCount(t *testing.T) {
	ctx := context.Background()

	profile := makeProfile("cat-shared-app")
	if err := testClient.Create(ctx, profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, profile) })

	// Create quota with enough room.
	quota := resource.MustParse("100Gi")
	for i := 0; i < 2; i++ {
		tn := &gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cat-multi-%d", i)},
			Spec: gentianov1alpha1.TenantSpec{
				DisplayName: fmt.Sprintf("Multi Tenant %d", i),
				Domain:      fmt.Sprintf("multi%d.example.com", i),
				AdminEmail:  fmt.Sprintf("admin@multi%d.example.com", i),
				Quotas: &gentianov1alpha1.TenantQuotas{
					MaxApps: 5,
					Storage: &quota,
				},
				Apps: []gentianov1alpha1.TenantApp{{Profile: "cat-shared-app"}},
			},
		}
		if err := testClient.Create(ctx, tn); err != nil {
			t.Fatalf("create Tenant %d: %v", i, err)
		}
		idx := i
		t.Cleanup(func() {
			_ = testClient.Delete(ctx, &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cat-multi-%d", idx)}})
		})
	}

	// Wait until InstalledCount == 2.
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		var cat gentianov1alpha1.AppCatalogue
		if err := testClient.Get(ctx, types.NamespacedName{Name: "default"}, &cat); err != nil {
			return false, nil
		}
		for _, e := range cat.Status.Apps {
			if e.Name == "cat-shared-app" {
				return e.InstalledCount == 2, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("InstalledCount did not reach 2: %v", err)
	}
}
