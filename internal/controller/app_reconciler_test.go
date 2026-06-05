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
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// appClaimTestGVK is the GVK for App claims created by the operator. It matches
// the CRD registered in the test environment (config/crd/gentianos.io_apps.yaml).
var appClaimTestGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "App",
}

// newAppProfile builds an AppProfile with the crossplane DeploymentMethod and
// optional ValueMapping.
func newAppProfile(name string, vm *gentianov1alpha1.ValueMapping) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      name,
			DeploymentMethod: gentianov1alpha1.DeploymentMethodCrossplane,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       name,
				Version:    "1.2.3",
			},
			ValueMapping: vm,
		},
	}
}

// TestApps_NoApps verifies that a Tenant with no apps skips provisioning and
// sets AppsReady=True with reason NoAppsConfigured.
func TestApps_NoApps(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "noapps"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "No Apps Co",
			Domain:      "noapps.example.com",
			AdminEmail:  "admin@noapps.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, tenantReadyTimeout, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "noapps"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	var cond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "AppsReady" {
			cond = &updated.Status.Conditions[i]
			break
		}
	}
	if cond == nil {
		t.Fatal("expected AppsReady condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected AppsReady=True, got %v", cond.Status)
	}
	if cond.Reason != "NoAppsConfigured" {
		t.Errorf("expected reason NoAppsConfigured, got %q", cond.Reason)
	}
}

// TestApps_CreatesAppClaim verifies that a Tenant with a single app creates one
// App claim in the tenant namespace with the correct spec (profileRef, tenantNamespace,
// domain) and labels.
func TestApps_CreatesAppClaim(t *testing.T) {
	t.Parallel()
	profile := newAppProfile("my-app", nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "single-app"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Single App Co",
			Domain:      "single-app.example.com",
			AdminEmail:  "admin@single-app.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "my-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	var claim *unstructured.Unstructured
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(appClaimTestGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "my-app", Namespace: "tenant-single-app"}, obj)
		if err == nil {
			claim = obj
		}
		return err == nil
	})

	if claim.GetLabels()["gentianos.io/tenant"] != "single-app" {
		t.Errorf("expected tenant label 'single-app', got %q", claim.GetLabels()["gentianos.io/tenant"])
	}
	if claim.GetLabels()["gentianos.io/app"] != "my-app" {
		t.Errorf("expected app label 'my-app', got %q", claim.GetLabels()["gentianos.io/app"])
	}

	profileRef, _, _ := unstructured.NestedString(claim.Object, "spec", "profileRef", "name")
	if profileRef != "my-app" {
		t.Errorf("expected spec.profileRef.name my-app, got %q", profileRef)
	}
	tenantNS, _, _ := unstructured.NestedString(claim.Object, "spec", "tenantNamespace")
	if tenantNS != "tenant-single-app" {
		t.Errorf("expected spec.tenantNamespace tenant-single-app, got %q", tenantNS)
	}
	domain, _, _ := unstructured.NestedString(claim.Object, "spec", "domain")
	if domain != "single-app.example.com" {
		t.Errorf("expected spec.domain single-app.example.com, got %q", domain)
	}
}

// TestApps_MultipleApps verifies that a Tenant with 3 apps creates 3 separate
// App claims in the tenant namespace, one per app.
func TestApps_MultipleApps(t *testing.T) {
	t.Parallel()
	appNames := []string{"alpha", "beta", "gamma"}
	for _, name := range appNames {
		profile := newAppProfile(name, nil)
		if err := testClient.Create(context.Background(), profile); err != nil {
			t.Fatalf("create AppProfile %s: %v", name, err)
		}
		n := name
		t.Cleanup(func() {
			_ = testClient.Delete(context.Background(), &gentianov1alpha1.AppProfile{
				ObjectMeta: metav1.ObjectMeta{Name: n},
			})
		})
	}

	var tenantApps []gentianov1alpha1.TenantApp
	for _, name := range appNames {
		tenantApps = append(tenantApps, gentianov1alpha1.TenantApp{Profile: name})
	}

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-app"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Multi App Co",
			Domain:      "multi-app.example.com",
			AdminEmail:  "admin@multi-app.example.com",
			Apps:        tenantApps,
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	for _, name := range appNames {
		n := name
		waitFor(t, 15*time.Second, func() bool {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(appClaimTestGVK)
			return testClient.Get(context.Background(),
				types.NamespacedName{Name: n, Namespace: "tenant-multi-app"}, obj) == nil
		})
	}
}

// TestApps_DeleteRemovesAppClaims verifies that deleting a Tenant removes all
// App claims from the tenant namespace.
func TestApps_DeleteRemovesAppClaims(t *testing.T) {
	t.Parallel()
	profile := newAppProfile("del-app", nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "del-tenant"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Del Tenant",
			Domain:         "del.example.com",
			AdminEmail:     "admin@del.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps:           []gentianov1alpha1.TenantApp{{Profile: "del-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for App claim to appear.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(appClaimTestGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "del-app", Namespace: "tenant-del-tenant"}, obj) == nil
	})

	// Delete the tenant.
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	// deleteIdentity and deleteLDAP run before deleteAppDeployment; mark their jobs.
	go markJobCompleteWhenReady("keycloak-realm-delete-del-tenant", "platform-kernel")
	go markJobCompleteWhenReady("ldap-ou-delete-del-tenant", "platform-kernel")
	go markJobCompleteWhenReady("nc-group-delete-del-tenant", "platform-kernel")

	// App claim should be removed.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(appClaimTestGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "del-app", Namespace: "tenant-del-tenant"}, obj)
		return err != nil // NotFound means it was deleted
	})
}

// TestApps_RemoveAppCleansUpClaim verifies that removing an app from
// tenant.spec.apps triggers deletion of the corresponding App claim while
// leaving other apps' claims intact.
func TestApps_RemoveAppCleansUpClaim(t *testing.T) {
	t.Parallel()
	profileA := newAppProfile("keep-app", nil)
	profileB := newAppProfile("remove-app", nil)
	if err := testClient.Create(context.Background(), profileA); err != nil {
		t.Fatalf("create AppProfile keep-app: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profileA) })
	if err := testClient.Create(context.Background(), profileB); err != nil {
		t.Fatalf("create AppProfile remove-app: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profileB) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "rm-app-tenant"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Remove App Tenant",
			Domain:      "rmapp.example.com",
			AdminEmail:  "admin@rmapp.example.com",
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "keep-app"},
				{Profile: "remove-app"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for both App claims to appear in the tenant namespace.
	for _, name := range []string{"keep-app", "remove-app"} {
		n := name
		waitFor(t, 15*time.Second, func() bool {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(appClaimTestGVK)
			return testClient.Get(context.Background(),
				types.NamespacedName{Name: n, Namespace: "tenant-rm-app-tenant"}, obj) == nil
		})
	}

	// Remove "remove-app" from the tenant's apps list.
	// Retry on conflict: the reconciler may have updated the tenant between our
	// Get and Update calls, incrementing the resourceVersion.
	for {
		updated := &gentianov1alpha1.Tenant{}
		if err := testClient.Get(context.Background(), types.NamespacedName{Name: "rm-app-tenant"}, updated); err != nil {
			t.Fatalf("get tenant: %v", err)
		}
		updated.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "keep-app"}}
		if err := testClient.Update(context.Background(), updated); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			t.Fatalf("update tenant: %v", err)
		}
		break
	}

	// The removed app's claim should be cleaned up.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(appClaimTestGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "remove-app", Namespace: "tenant-rm-app-tenant"}, obj)
		return err != nil // NotFound means it was deleted
	})

	// The kept app's claim should still exist.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(appClaimTestGVK)
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "keep-app", Namespace: "tenant-rm-app-tenant"}, obj); err != nil {
		t.Errorf("expected keep-app App claim to still exist, got error: %v", err)
	}
}

// TestApps_OrphanCleanupSkipsCRsWithoutAppLabel verifies that the orphan cleanup
// does not delete App claims that share the tenant and managed-by labels but
// lack the gentianos.io/app label.
func TestApps_OrphanCleanupSkipsCRsWithoutAppLabel(t *testing.T) {
	t.Parallel()
	profile := newAppProfile("only-app", nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "skip-noapp"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Skip NoApp Tenant",
			Domain:      "skipnoapp.example.com",
			AdminEmail:  "admin@skipnoapp.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "only-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for the app's App claim to appear in the tenant namespace.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(appClaimTestGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "only-app", Namespace: "tenant-skip-noapp"}, obj) == nil
	})

	// Create a foreign App claim in the tenant namespace: same tenant+managed-by
	// labels but no gentianos.io/app label. The orphan cleanup must skip it.
	foreignClaim := &unstructured.Unstructured{}
	foreignClaim.SetGroupVersionKind(appClaimTestGVK)
	foreignClaim.SetName("foreign-no-app-label")
	foreignClaim.SetNamespace("tenant-skip-noapp")
	foreignClaim.SetLabels(map[string]string{
		"gentianos.io/tenant":          "skip-noapp",
		"app.kubernetes.io/managed-by": "gentian-os",
	})
	_ = unstructured.SetNestedField(foreignClaim.Object, "foreign-no-app-label", "spec", "profileRef", "name")
	if err := testClient.Create(context.Background(), foreignClaim); err != nil {
		t.Fatalf("create foreign claim: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), foreignClaim) })

	// Trigger a reconcile by updating the tenant (no-op change).
	// Retry on conflict: the reconciler may have updated the tenant between our
	// Get and Update calls, incrementing the resourceVersion.
	for {
		updated := &gentianov1alpha1.Tenant{}
		if err := testClient.Get(context.Background(), types.NamespacedName{Name: "skip-noapp"}, updated); err != nil {
			t.Fatalf("get tenant: %v", err)
		}
		updated.Spec.DisplayName = "Skip NoApp Tenant Updated"
		if err := testClient.Update(context.Background(), updated); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			t.Fatalf("update tenant: %v", err)
		}
		break
	}

	// Wait for reconcile to complete (the managed app claim remains).
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(appClaimTestGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "only-app", Namespace: "tenant-skip-noapp"}, obj) == nil
	})

	// The foreign claim without the app label must still exist.
	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(appClaimTestGVK)
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "foreign-no-app-label", Namespace: "tenant-skip-noapp"}, check); err != nil {
		t.Errorf("expected foreign claim without app label to survive orphan cleanup, got error: %v", err)
	}
}
