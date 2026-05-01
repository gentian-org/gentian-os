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
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var tofuTerraformGVK = schema.GroupVersionKind{
	Group:   "infra.contrib.fluxcd.io",
	Version: "v1alpha2",
	Kind:    "Terraform",
}

// newAppProfile builds an AppProfile with argocd DeploymentMethod and optional ValueMapping.
func newAppProfile(name string, vm *gentianov1alpha1.ValueMapping) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      name,
			DeploymentMethod: gentianov1alpha1.DeploymentMethodArgoCD,
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
	waitFor(t, 10*time.Second, func() bool {
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

// TestApps_CreatesApplicationCR verifies that a Tenant with a single app creates
// one ArgoCD Application CR in the argocd namespace with the correct chart source,
// destination namespace, and labels.
func TestApps_CreatesApplicationCR(t *testing.T) {
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

	var appCR *unstructured.Unstructured
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(argocdAppGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "app-single-app-my-app", Namespace: "argocd"}, obj)
		if err == nil {
			appCR = obj
		}
		return err == nil
	})

	if appCR.GetLabels()["gentianos.io/tenant"] != "single-app" {
		t.Errorf("expected tenant label 'single-app', got %q", appCR.GetLabels()["gentianos.io/tenant"])
	}
	if appCR.GetLabels()["gentianos.io/app"] != "my-app" {
		t.Errorf("expected app label 'my-app', got %q", appCR.GetLabels()["gentianos.io/app"])
	}

	repoURL, _, _ := unstructured.NestedString(appCR.Object, "spec", "source", "repoURL")
	if repoURL != "oci://charts.example.com" {
		t.Errorf("expected repoURL oci://charts.example.com, got %q", repoURL)
	}
	chart, _, _ := unstructured.NestedString(appCR.Object, "spec", "source", "chart")
	if chart != "my-app" {
		t.Errorf("expected chart my-app, got %q", chart)
	}
	version, _, _ := unstructured.NestedString(appCR.Object, "spec", "source", "targetRevision")
	if version != "1.2.3" {
		t.Errorf("expected version 1.2.3, got %q", version)
	}

	destNS, _, _ := unstructured.NestedString(appCR.Object, "spec", "destination", "namespace")
	if destNS != "tenant-single-app" {
		t.Errorf("expected destination namespace 'tenant-single-app', got %q", destNS)
	}
}

// TestApps_MultipleApps verifies that a Tenant with 3 apps creates 3 separate
// ArgoCD Application CRs in the argocd namespace, one per app.
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
			obj.SetGroupVersionKind(argocdAppGVK)
			return testClient.Get(context.Background(),
				types.NamespacedName{Name: "app-multi-app-" + n, Namespace: "argocd"}, obj) == nil
		})
	}
}

// TestApps_ValueMappingRendered verifies that OIDC and Database keys from the
// AppProfile's ValueMapping appear as helm values in the Application CR.
func TestApps_ValueMappingRendered(t *testing.T) {
	t.Parallel()
	vm := &gentianov1alpha1.ValueMapping{
		OIDC: &gentianov1alpha1.OIDCValueMapping{
			IssuerKey:   "oidc.issuer",
			ClientIDKey: "oidc.clientId",
		},
		Database: &gentianov1alpha1.DatabaseValueMapping{
			NameKey: "db.name",
			HostKey: "db.host",
		},
	}
	profile := newAppProfile("mapped-app", vm)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-tenant"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "VM Tenant",
			Domain:      "vm.example.com",
			AdminEmail:  "admin@vm.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "mapped-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	var helmValues string
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(argocdAppGVK)
		if err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "app-vm-tenant-mapped-app", Namespace: "argocd"}, obj); err != nil {
			return false
		}
		v, _, _ := unstructured.NestedString(obj.Object, "spec", "source", "helm", "values")
		helmValues = v
		return v != ""
	})

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(helmValues), &parsed); err != nil {
		t.Fatalf("helm values not valid JSON: %v - raw: %q", err, helmValues)
	}

	oidcMap, ok := parsed["oidc"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected 'oidc' nested map, got %T", parsed["oidc"])
	}
	if oidcMap["issuer"] == nil {
		t.Error("expected oidc.issuer to be set")
	}
	if oidcMap["clientId"] == nil {
		t.Error("expected oidc.clientId to be set")
	}

	dbMap, ok := parsed["db"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected 'db' nested map, got %T", parsed["db"])
	}
	if dbMap["name"] == nil {
		t.Error("expected db.name to be set")
	}
	if dbMap["host"] == nil {
		t.Error("expected db.host to be set")
	}
}

// TestApps_DeleteRemovesApplicationCRs verifies that deleting a Tenant removes
// all Application CRs from the argocd namespace.
func TestApps_DeleteRemovesApplicationCRs(t *testing.T) {
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

	// Wait for Application CR to appear.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(argocdAppGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "app-del-tenant-del-app", Namespace: "argocd"}, obj) == nil
	})

	// Delete the tenant.
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}

	// Application CR should be removed.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(argocdAppGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "app-del-tenant-del-app", Namespace: "argocd"}, obj)
		return err != nil // NotFound means it was deleted
	})
}

// newTofuAppProfile builds an AppProfile with the tofu-controller DeploymentMethod
// and an optional ValueMapping, matching the real-world openproject / ox-appsuite profiles.
func newTofuAppProfile(name string, vm *gentianov1alpha1.ValueMapping) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      name,
			DeploymentMethod: gentianov1alpha1.DeploymentMethodTofuController,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       name,
				Version:    "2.0.0",
			},
			ValueMapping: vm,
		},
	}
}

// TestTofuApps_CreatesTerraformCR verifies that a tenant with a tofu-controller app
// creates exactly one Terraform CR in tofu-system with the correct labels, vars (chart
// reference and tenant/app identifiers), sourceRef, and approvePlan=auto.
func TestTofuApps_CreatesTerraformCR(t *testing.T) {
	t.Parallel()
	profile := newTofuAppProfile("tofu-app", nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "tofu-tenant"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Tofu Tenant",
			Domain:      "tofu.example.com",
			AdminEmail:  "admin@tofu.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "tofu-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	var tfCR *unstructured.Unstructured
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tofuTerraformGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "tf-tofu-tenant-tofu-app", Namespace: "tofu-system"}, obj)
		if err == nil {
			tfCR = obj
		}
		return err == nil
	})

	// Labels
	if tfCR.GetLabels()["gentianos.io/tenant"] != "tofu-tenant" {
		t.Errorf("expected tenant label 'tofu-tenant', got %q", tfCR.GetLabels()["gentianos.io/tenant"])
	}
	if tfCR.GetLabels()["gentianos.io/app"] != "tofu-app" {
		t.Errorf("expected app label 'tofu-app', got %q", tfCR.GetLabels()["gentianos.io/app"])
	}

	// spec fields
	approvePlan, _, _ := unstructured.NestedString(tfCR.Object, "spec", "approvePlan")
	if approvePlan != "auto" {
		t.Errorf("expected approvePlan=auto, got %q", approvePlan)
	}
	modPath, _, _ := unstructured.NestedString(tfCR.Object, "spec", "path")
	if modPath != "kernel/tofu/tenant/app-workspace" {
		t.Errorf("expected path kernel/tofu/tenant/app-workspace, got %q", modPath)
	}
	srcKind, _, _ := unstructured.NestedString(tfCR.Object, "spec", "sourceRef", "kind")
	if srcKind != "GitRepository" {
		t.Errorf("expected sourceRef.kind=GitRepository, got %q", srcKind)
	}
	srcName, _, _ := unstructured.NestedString(tfCR.Object, "spec", "sourceRef", "name")
	if srcName != "gentian-server" {
		t.Errorf("expected sourceRef.name=gentian-server, got %q", srcName)
	}
	saName, _, _ := unstructured.NestedString(tfCR.Object, "spec", "serviceAccountName")
	if saName != "tf-runner" {
		t.Errorf("expected serviceAccountName=tf-runner, got %q", saName)
	}

	// spec.vars should contain tenant_name, app_name, namespace, chart_*
	vars, _, _ := unstructured.NestedSlice(tfCR.Object, "spec", "vars")
	varMap := make(map[string]string)
	for _, v := range vars {
		vm, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := vm["name"].(string)
		value, _ := vm["value"].(string)
		varMap[name] = value
	}
	checks := map[string]string{
		"tenant_name":      "tofu-tenant",
		"app_name":         "tofu-app",
		"namespace":        "tenant-tofu-tenant",
		"chart_repository": "oci://charts.example.com",
		"chart_name":       "tofu-app",
		"chart_version":    "2.0.0",
	}
	for k, want := range checks {
		if got := varMap[k]; got != want {
			t.Errorf("var %s: want %q, got %q", k, want, got)
		}
	}
}

// TestTofuApps_ValueMappingVars verifies that when an AppProfile has a ValueMapping,
// the Terraform CR receives the corresponding vm_* variables for the module to wire.
func TestTofuApps_ValueMappingVars(t *testing.T) {
	t.Parallel()
	vm := &gentianov1alpha1.ValueMapping{
		OIDC: &gentianov1alpha1.OIDCValueMapping{
			IssuerKey:       "oidc.issuer",
			ClientIDKey:     "oidc.clientId",
			ClientSecretKey: "oidc.clientSecret",
		},
		Database: &gentianov1alpha1.DatabaseValueMapping{
			HostKey:     "db.host",
			PasswordKey: "db.password",
		},
	}
	profile := newTofuAppProfile("tofu-vm-app", vm)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "tofu-vm-tenant"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Tofu VM Tenant",
			Domain:      "tofuvm.example.com",
			AdminEmail:  "admin@tofuvm.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "tofu-vm-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	var tfCR *unstructured.Unstructured
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tofuTerraformGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "tf-tofu-vm-tenant-tofu-vm-app", Namespace: "tofu-system"}, obj)
		if err == nil {
			tfCR = obj
		}
		return err == nil
	})

	vars, _, _ := unstructured.NestedSlice(tfCR.Object, "spec", "vars")
	varMap := make(map[string]string)
	for _, v := range vars {
		vm, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := vm["name"].(string)
		value, _ := vm["value"].(string)
		varMap[name] = value
	}

	vmChecks := map[string]string{
		"vm_oidc_issuer_key":        "oidc.issuer",
		"vm_oidc_client_id_key":     "oidc.clientId",
		"vm_oidc_client_secret_key": "oidc.clientSecret",
		"vm_db_host_key":            "db.host",
		"vm_db_password_key":        "db.password",
	}
	for k, want := range vmChecks {
		if got := varMap[k]; got != want {
			t.Errorf("var %s: want %q, got %q", k, want, got)
		}
	}
	// Keys with empty valueMapping should NOT appear.
	for _, absent := range []string{"vm_db_port_key", "vm_db_name_key", "vm_s3_bucket_key"} {
		if _, found := varMap[absent]; found {
			t.Errorf("var %s should not be present", absent)
		}
	}
}

// TestTofuApps_AppSecretsVarIsStructuredMap verifies that AppProfile.spec.appSecrets
// is rendered into the Terraform CR's spec.vars as a structured JSON object
// (map[string]interface{}), NOT a JSON-encoded string. The app-workspace module
// declares `variable "app_secrets" { type = map(string) }`; passing a string
// would cause OpenTofu to reject the Plan with
// "map of string required, but have string".
func TestTofuApps_AppSecretsVarIsStructuredMap(t *testing.T) {
	t.Parallel()
	profile := newTofuAppProfile("tofu-appsecrets-app", nil)
	profile.Spec.AppSecrets = []gentianov1alpha1.AppSecret{
		{Name: "registration_shared_secret", ValuePath: "configuration.homeserver.registrationSharedSecret"},
		{Name: "intercom_as_token", ValuePath: "configuration.homeserver.appServiceConfigs[0].as_token"},
	}
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "tofu-appsecrets-tenant"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Tofu AppSecrets Tenant",
			Domain:      "appsecrets.example.com",
			AdminEmail:  "admin@appsecrets.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "tofu-appsecrets-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	var tfCR *unstructured.Unstructured
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tofuTerraformGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "tf-tofu-appsecrets-tenant-tofu-appsecrets-app", Namespace: "tofu-system"}, obj)
		if err == nil {
			tfCR = obj
		}
		return err == nil
	})
	if tfCR == nil {
		t.Fatal("Terraform CR not created")
	}

	vars, _, _ := unstructured.NestedSlice(tfCR.Object, "spec", "vars")
	var found bool
	for _, v := range vars {
		vm, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _ := vm["name"].(string); name != "app_secrets" {
			continue
		}
		found = true
		// Must be a structured map, not a string.
		if _, isString := vm["value"].(string); isString {
			t.Fatalf("app_secrets value is a string; must be a structured map for Tofu type=map(string)")
		}
		m, ok := vm["value"].(map[string]interface{})
		if !ok {
			t.Fatalf("app_secrets value: want map[string]interface{}, got %T", vm["value"])
		}
		if got, _ := m["registration_shared_secret"].(string); got != "configuration.homeserver.registrationSharedSecret" {
			t.Errorf("registration_shared_secret: got %q", got)
		}
		if got, _ := m["intercom_as_token"].(string); got != "configuration.homeserver.appServiceConfigs[0].as_token" {
			t.Errorf("intercom_as_token: got %q", got)
		}
	}
	if !found {
		t.Fatal("app_secrets var not present in Terraform CR")
	}
}

// TestTofuApps_NoArgocdCR verifies that a tofu-controller app does NOT create an
// ArgoCD Application CR — only a Terraform CR in tofu-system.
func TestTofuApps_NoArgocdCR(t *testing.T) {
	t.Parallel()
	profile := newTofuAppProfile("tofu-only-app", nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "tofu-only-tenant"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Tofu Only Co",
			Domain:      "tofuonly.example.com",
			AdminEmail:  "admin@tofuonly.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "tofu-only-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for Terraform CR to appear.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tofuTerraformGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "tf-tofu-only-tenant-tofu-only-app", Namespace: "tofu-system"}, obj) == nil
	})

	// ArgoCD Application CR must NOT exist.
	appCR := &unstructured.Unstructured{}
	appCR.SetGroupVersionKind(argocdAppGVK)
	err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "app-tofu-only-tenant-tofu-only-app", Namespace: "argocd"}, appCR)
	if err == nil {
		t.Error("expected NO ArgoCD Application CR for a tofu-controller app, but one was found")
	}
}

// TestTofuApps_DeleteRemovesTerraformCR verifies that deleting a Tenant removes
// the Terraform CR from tofu-system.
func TestTofuApps_DeleteRemovesTerraformCR(t *testing.T) {
	t.Parallel()
	_ = json.Marshal // keep json import used
	profile := newTofuAppProfile("tofu-del-app", nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "tofu-del-tenant"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Tofu Del Tenant",
			Domain:         "tofudel.example.com",
			AdminEmail:     "admin@tofudel.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps:           []gentianov1alpha1.TenantApp{{Profile: "tofu-del-app"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for Terraform CR to appear.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tofuTerraformGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "tf-tofu-del-tenant-tofu-del-app", Namespace: "tofu-system"}, obj) == nil
	})

	// Delete the tenant.
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}

	// Terraform CR should be removed.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tofuTerraformGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "tf-tofu-del-tenant-tofu-del-app", Namespace: "tofu-system"}, obj)
		return err != nil // NotFound means it was deleted
	})
}

// TestApps_RemoveAppCleansUpApplicationCR verifies that removing an app from
// tenant.spec.apps triggers deletion of the corresponding ArgoCD Application CR
// while leaving other apps' CRs intact.
func TestApps_RemoveAppCleansUpApplicationCR(t *testing.T) {
	t.Parallel()
	// Create two AppProfiles.
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

	// Wait for both Application CRs to appear.
	for _, name := range []string{"keep-app", "remove-app"} {
		n := name
		waitFor(t, 15*time.Second, func() bool {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(argocdAppGVK)
			return testClient.Get(context.Background(),
				types.NamespacedName{Name: "app-rm-app-tenant-" + n, Namespace: "argocd"}, obj) == nil
		})
	}

	// Remove "remove-app" from the tenant's apps list.
	updated := &gentianov1alpha1.Tenant{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: "rm-app-tenant"}, updated); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	updated.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "keep-app"}}
	if err := testClient.Update(context.Background(), updated); err != nil {
		t.Fatalf("update tenant: %v", err)
	}

	// The removed app's Application CR should be cleaned up.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(argocdAppGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "app-rm-app-tenant-remove-app", Namespace: "argocd"}, obj)
		return err != nil // NotFound means it was deleted
	})

	// The kept app's Application CR should still exist.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdAppGVK)
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "app-rm-app-tenant-keep-app", Namespace: "argocd"}, obj); err != nil {
		t.Errorf("expected keep-app Application CR to still exist, got error: %v", err)
	}
}

// TestTofuApps_RemoveAppCleansUpTerraformCR verifies that removing a tofu-controller
// app from tenant.spec.apps triggers deletion of the corresponding Terraform CR.
func TestTofuApps_RemoveAppCleansUpTerraformCR(t *testing.T) {
	t.Parallel()
	// Create two tofu AppProfiles.
	profileA := newTofuAppProfile("tofu-keep", nil)
	profileB := newTofuAppProfile("tofu-remove", nil)
	if err := testClient.Create(context.Background(), profileA); err != nil {
		t.Fatalf("create AppProfile tofu-keep: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profileA) })
	if err := testClient.Create(context.Background(), profileB); err != nil {
		t.Fatalf("create AppProfile tofu-remove: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profileB) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "tofu-rm-tenant"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Tofu Remove Tenant",
			Domain:      "tofurm.example.com",
			AdminEmail:  "admin@tofurm.example.com",
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "tofu-keep"},
				{Profile: "tofu-remove"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for both Terraform CRs to appear.
	for _, name := range []string{"tofu-keep", "tofu-remove"} {
		n := name
		waitFor(t, 15*time.Second, func() bool {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(tofuTerraformGVK)
			return testClient.Get(context.Background(),
				types.NamespacedName{Name: "tf-tofu-rm-tenant-" + n, Namespace: "tofu-system"}, obj) == nil
		})
	}

	// Remove "tofu-remove" from the tenant's apps list.
	updated := &gentianov1alpha1.Tenant{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: "tofu-rm-tenant"}, updated); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	updated.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "tofu-keep"}}
	if err := testClient.Update(context.Background(), updated); err != nil {
		t.Fatalf("update tenant: %v", err)
	}

	// The removed app's Terraform CR should be cleaned up.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tofuTerraformGVK)
		err := testClient.Get(context.Background(),
			types.NamespacedName{Name: "tf-tofu-rm-tenant-tofu-remove", Namespace: "tofu-system"}, obj)
		return err != nil // NotFound means it was deleted
	})

	// The kept app's Terraform CR should still exist.
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(tofuTerraformGVK)
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "tf-tofu-rm-tenant-tofu-keep", Namespace: "tofu-system"}, obj); err != nil {
		t.Errorf("expected tofu-keep Terraform CR to still exist, got error: %v", err)
	}
}

// TestApps_OrphanCleanupSkipsCRsWithoutAppLabel verifies that the orphan cleanup
// does not delete Application CRs that share the tenant and managed-by labels
// but lack the gentianos.io/app label (e.g. Memcached CRs from the cache reconciler).
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
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "only-app"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	// Wait for the app's Application CR to appear.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(argocdAppGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "app-skip-noapp-only-app", Namespace: "argocd"}, obj) == nil
	})

	// Simulate a CR created by another reconciler (e.g. cache): same tenant+managed-by
	// labels but NO gentianos.io/app label.
	foreignCR := &unstructured.Unstructured{}
	foreignCR.SetGroupVersionKind(argocdAppGVK)
	foreignCR.SetName("memcached-skip-noapp")
	foreignCR.SetNamespace("argocd")
	foreignCR.SetLabels(map[string]string{
		"gentianos.io/tenant":          "skip-noapp",
		"app.kubernetes.io/managed-by": "gentian-os",
	})
	_ = unstructured.SetNestedField(foreignCR.Object, "default", "spec", "project")
	_ = unstructured.SetNestedField(foreignCR.Object, "https://kubernetes.default.svc", "spec", "destination", "server")
	if err := testClient.Create(context.Background(), foreignCR); err != nil {
		t.Fatalf("create foreign CR: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), foreignCR) })

	// Trigger a reconcile by updating the tenant (no-op change).
	updated := &gentianov1alpha1.Tenant{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: "skip-noapp"}, updated); err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	updated.Spec.DisplayName = "Skip NoApp Tenant Updated"
	if err := testClient.Update(context.Background(), updated); err != nil {
		t.Fatalf("update tenant: %v", err)
	}

	// Wait for reconcile to complete (the app CR remains).
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(argocdAppGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "app-skip-noapp-only-app", Namespace: "argocd"}, obj) == nil
	})

	// The foreign CR (without app label) must still exist.
	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(argocdAppGVK)
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "memcached-skip-noapp", Namespace: "argocd"}, check); err != nil {
		t.Errorf("expected foreign CR without app label to survive orphan cleanup, got error: %v", err)
	}
}

// TestTofuApps_UninstallCleansUpHelmWorkloads verifies the full uninstall path for a
// Pattern B app: when a Tenant is deleted the reconciler must delete Helm workload
// resources (by app.kubernetes.io/instance label) AND the Helm release tracking
// secrets in the tenant namespace, in addition to the Terraform CR itself.
//
// This covers the tofu-controller finalizer-panic workaround: terraform destroy
// cannot run, so the operator must clean up the deployed resources itself.
func TestTofuApps_UninstallCleansUpHelmWorkloads(t *testing.T) {
	t.Parallel()
	const (
		profileName = "collabora-cleanup"
		tenantName  = "tofu-cleanup"
		tenantNs    = "tenant-tofu-cleanup"
		tfCRName    = "tf-tofu-cleanup-collabora-cleanup"
		releaseName = profileName // appLabel on the Terraform CR = profile name
	)

	profile := newTofuAppProfile(profileName, nil)
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Tofu Cleanup Tenant",
			Domain:         "tofucleanup.example.com",
			AdminEmail:     "admin@tofucleanup.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps:           []gentianov1alpha1.TenantApp{{Profile: profileName}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for the Terraform CR to appear in tofu-system.
	waitFor(t, 15*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tofuTerraformGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: tfCRName, Namespace: "tofu-system"}, obj) == nil
	})

	// Wait for the tenant namespace to be created by the namespace reconciler.
	waitFor(t, 15*time.Second, func() bool {
		ns := &unstructured.Unstructured{}
		ns.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"})
		return testClient.Get(context.Background(), types.NamespacedName{Name: tenantNs}, ns) == nil
	})

	// Simulate a Helm-managed ConfigMap in the tenant namespace (represents any
	// workload resource: Deployment, Service, etc.).
	helmCM := &unstructured.Unstructured{}
	helmCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	helmCM.SetName("collabora-cleanup-config")
	helmCM.SetNamespace(tenantNs)
	helmCM.SetLabels(map[string]string{
		"app.kubernetes.io/instance":   releaseName,
		"app.kubernetes.io/managed-by": "Helm",
	})
	if err := testClient.Create(context.Background(), helmCM); err != nil {
		t.Fatalf("create Helm ConfigMap: %v", err)
	}

	// Simulate a Helm release tracking secret in the tenant namespace.
	helmSecret := &unstructured.Unstructured{}
	helmSecret.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"})
	helmSecret.SetName("sh.helm.release.v1." + releaseName + ".v1")
	helmSecret.SetNamespace(tenantNs)
	helmSecret.SetLabels(map[string]string{
		"owner":  "helm",
		"name":   releaseName,
		"status": "deployed",
	})
	if err := testClient.Create(context.Background(), helmSecret); err != nil {
		t.Fatalf("create Helm release secret: %v", err)
	}

	// Set the tofu-controller finalizer on the Terraform CR to simulate the
	// stuck-finalizer scenario that prevents terraform destroy from running.
	tfCR := &unstructured.Unstructured{}
	tfCR.SetGroupVersionKind(tofuTerraformGVK)
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: tfCRName, Namespace: "tofu-system"}, tfCR); err != nil {
		t.Fatalf("get Terraform CR: %v", err)
	}
	tfCR.SetFinalizers([]string{"finalizers.tf.contrib.fluxcd.io"})
	if err := testClient.Update(context.Background(), tfCR); err != nil {
		t.Fatalf("set finalizer on Terraform CR: %v", err)
	}

	// Delete the tenant — triggers deleteTerraformCR via deleteAppDeployment.
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}

	// Terraform CR must be gone (finalizer stripped + CR deleted).
	waitFor(t, 20*time.Second, func() bool {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(tofuTerraformGVK)
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: tfCRName, Namespace: "tofu-system"}, obj) != nil
	})

	// Helm workload ConfigMap must be deleted.
	checkCM := &unstructured.Unstructured{}
	checkCM.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "collabora-cleanup-config", Namespace: tenantNs}, checkCM); err == nil {
		t.Error("expected Helm workload ConfigMap to be deleted, but it still exists")
	}

	// Helm release tracking secret must be deleted.
	checkSecret := &unstructured.Unstructured{}
	checkSecret.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"})
	if err := testClient.Get(context.Background(),
		types.NamespacedName{Name: "sh.helm.release.v1." + releaseName + ".v1", Namespace: tenantNs}, checkSecret); err == nil {
		t.Error("expected Helm release secret to be deleted, but it still exists")
	}
}
