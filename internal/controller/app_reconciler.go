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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	conditionAppsReady = "AppsReady"

	// tofu-controller constants.
	tofuSystemNamespace   = "tofu-system"
	tofuGitRepositoryName = "gentian-server"
	tofuModulePath        = "kernel/tofu/tenant/app-workspace"

	// MinIO state backend settings. customConfiguration in the Terraform CR fully
	// overrides the backend block in the Terraform modules, so all required
	// S3 attributes must be supplied here.
	tofuStateBucket   = "tofu-state"
	tofuStateEndpoint = "http://minio-dev.gentian-infra-dev.svc.cluster.local:9000"
)

var terraformGVK = schema.GroupVersionKind{
	Group:   "infra.contrib.fluxcd.io",
	Version: "v1alpha2",
	Kind:    "Terraform",
}

// ensureAppDeployment creates or reconciles one ArgoCD Application CR (Pattern A)
// or Terraform CR (Pattern B) per app declared in tenant.Spec.Apps. It also
// cleans up orphaned CRs for apps that have been removed from the spec.
//
// Returns a non-zero RequeueAfter when any Application is not yet Healthy.
func (r *TenantReconciler) ensureAppDeployment(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	// Build the set of desired app profile names from the current spec.
	desiredApps := make(map[string]struct{}, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		desiredApps[app.Profile] = struct{}{}
	}

	if len(tenant.Spec.Apps) == 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionTrue, "NoAppsConfigured", "No applications are configured for this tenant")
	}

	allHealthy := true

	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "ProfileNotFound",
					fmt.Sprintf("AppProfile %q not found", app.Profile))
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}

		// Pattern B (tofu-controller): create a Terraform CR in tofu-system.
		if profile.Spec.DeploymentMethod == gentianov1alpha1.DeploymentMethodTofuController {
			ready, err := r.ensureTerraformCR(ctx, tenant, app, profile)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("ensure Terraform CR for app %s: %w", app.Profile, err)
			}
			if !ready {
				allHealthy = false
			}
			continue
		}

		healthy, err := r.ensureAppApplication(ctx, tenant, app, profile)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Application for app %s: %w", app.Profile, err)
		}
		if !healthy {
			allHealthy = false
		}
	}

	// Clean up orphaned CRs for apps that have been removed from spec.apps.
	if err := r.cleanupOrphanedAppCRs(ctx, tenant, desiredApps); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup orphaned app CRs: %w", err)
	}

	if len(tenant.Spec.Apps) > 0 && !allHealthy {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "Provisioning", "Waiting for Application CRs to become Healthy")
		return ctrl.Result{}, nil
	}

	if len(tenant.Spec.Apps) > 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionTrue, "Provisioned", "All Application CRs are Healthy")
	}
	return ctrl.Result{}, nil
}

// cleanupOrphanedAppCRs lists all ArgoCD Application and Terraform CRs managed
// by this operator for the given tenant, and deletes any whose app label is not
// in the desiredApps set. This ensures that removing an app from tenant.spec.apps
// triggers proper cleanup of the corresponding CR.
func (r *TenantReconciler) cleanupOrphanedAppCRs(ctx context.Context, tenant *gentianov1alpha1.Tenant, desiredApps map[string]struct{}) error {
	labelSelector := client.MatchingLabels{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	}

	// Clean up orphaned ArgoCD Application CRs (Pattern A).
	appList := &unstructured.UnstructuredList{}
	appList.SetGroupVersionKind(argocdApplicationGVK)
	if err := r.List(ctx, appList, client.InNamespace(argocdNamespace), labelSelector); err != nil {
		return fmt.Errorf("list Application CRs: %w", err)
	}
	for i := range appList.Items {
		appName := appList.Items[i].GetLabels()[appLabel]
		if _, desired := desiredApps[appName]; !desired {
			if err := r.Delete(ctx, &appList.Items[i]); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("delete orphaned Application CR %s: %w", appList.Items[i].GetName(), err)
			}
		}
	}

	// Clean up orphaned Terraform CRs (Pattern B).
	tfList := &unstructured.UnstructuredList{}
	tfList.SetGroupVersionKind(terraformGVK)
	if err := r.List(ctx, tfList, client.InNamespace(tofuSystemNamespace), labelSelector); err != nil {
		return fmt.Errorf("list Terraform CRs: %w", err)
	}
	for i := range tfList.Items {
		appName := tfList.Items[i].GetLabels()[appLabel]
		if _, desired := desiredApps[appName]; !desired {
			if err := r.Delete(ctx, &tfList.Items[i]); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("delete orphaned Terraform CR %s: %w", tfList.Items[i].GetName(), err)
			}
		}
	}

	return nil
}

// ensureAppApplication creates (or checks health of) the ArgoCD Application CR
// for a single app within a tenant. Returns true when the Application is Healthy.
func (r *TenantReconciler) ensureAppApplication(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	app gentianov1alpha1.TenantApp,
	profile *gentianov1alpha1.AppProfile,
) (bool, error) {
	appCRName := appApplicationName(tenant.Name, app.Profile)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdApplicationGVK)
	err := r.Get(ctx, types.NamespacedName{Name: appCRName, Namespace: argocdNamespace}, obj)
	if errors.IsNotFound(err) {
		desired, buildErr := buildAppApplication(tenant, app, profile)
		if buildErr != nil {
			return false, fmt.Errorf("build Application CR for %s: %w", app.Profile, buildErr)
		}
		return false, r.Create(ctx, desired)
	}
	if err != nil {
		return false, err
	}
	return argocdApplicationIsHealthy(obj), nil
}

// deleteAppDeployment removes all ArgoCD Application CRs and Terraform CRs created
// for the tenant's apps. Uses label-based listing to find all managed CRs, so it
// works even if apps have already been removed from tenant.spec.apps.
// Apps are ephemeral workload resources, so they are always deleted regardless of
// the tenant's DeletionPolicy; ArgoCD cascades deletion of deployed Helm releases;
// the tofu-controller destroys its managed Helm releases when
// destroyResourcesOnDeletion is true.
func (r *TenantReconciler) deleteAppDeployment(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	labelSelector := client.MatchingLabels{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	}

	// Delete all ArgoCD Application CRs (Pattern A).
	appList := &unstructured.UnstructuredList{}
	appList.SetGroupVersionKind(argocdApplicationGVK)
	if err := r.List(ctx, appList, client.InNamespace(argocdNamespace), labelSelector); err != nil {
		return fmt.Errorf("list Application CRs for tenant %s: %w", tenant.Name, err)
	}
	for i := range appList.Items {
		if err := r.Delete(ctx, &appList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Application CR %s: %w", appList.Items[i].GetName(), err)
		}
	}

	// Delete all Terraform CRs (Pattern B).
	tfList := &unstructured.UnstructuredList{}
	tfList.SetGroupVersionKind(terraformGVK)
	if err := r.List(ctx, tfList, client.InNamespace(tofuSystemNamespace), labelSelector); err != nil {
		return fmt.Errorf("list Terraform CRs for tenant %s: %w", tenant.Name, err)
	}
	for i := range tfList.Items {
		if err := r.Delete(ctx, &tfList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Terraform CR %s: %w", tfList.Items[i].GetName(), err)
		}
	}

	return nil
}

// ensureTerraformCR creates or updates the tofu-controller Terraform CR for a
// single Pattern B app within a tenant. The CR is placed in tofuSystemNamespace so
// the tofu-controller can reconcile it. On every reconcile the mutable spec fields
// (backendConfig, vars, path) are patched to reflect the current desired state so
// that AppProfile changes propagate and the backend key stays correct.
// Returns true when the Terraform CR reports Ready=True.
func (r *TenantReconciler) ensureTerraformCR(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	app gentianov1alpha1.TenantApp,
	profile *gentianov1alpha1.AppProfile,
) (bool, error) {
	crName := terraformCRName(tenant.Name, app.Profile)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(terraformGVK)
	err := r.Get(ctx, types.NamespacedName{Name: crName, Namespace: tofuSystemNamespace}, obj)
	if errors.IsNotFound(err) {
		desired, buildErr := buildTerraformCR(tenant, app, profile)
		if buildErr != nil {
			return false, fmt.Errorf("build Terraform CR for %s: %w", app.Profile, buildErr)
		}
		return false, r.Create(ctx, desired)
	}
	if err != nil {
		return false, err
	}

	// CR exists: patch mutable spec fields so AppProfile changes and backend
	// config corrections are applied without deleting and recreating the CR.
	desired, buildErr := buildTerraformCR(tenant, app, profile)
	if buildErr != nil {
		return false, fmt.Errorf("build Terraform CR for %s: %w", app.Profile, buildErr)
	}
	base := obj.DeepCopy()
	// Propagate backendConfig, vars, and path from desired → existing.
	if bc, found, _ := unstructured.NestedMap(desired.Object, "spec", "backendConfig"); found {
		_ = unstructured.SetNestedMap(obj.Object, bc, "spec", "backendConfig")
	}
	// Remove legacy disable:true if present (replaced by customConfiguration).
	unstructured.RemoveNestedField(obj.Object, "spec", "backendConfig", "disable")
	if v, found, _ := unstructured.NestedSlice(desired.Object, "spec", "vars"); found {
		_ = unstructured.SetNestedSlice(obj.Object, v, "spec", "vars")
	}
	if p, found, _ := unstructured.NestedString(desired.Object, "spec", "path"); found {
		_ = unstructured.SetNestedField(obj.Object, p, "spec", "path")
	}
	if err := r.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return false, fmt.Errorf("patch Terraform CR for %s: %w", app.Profile, err)
	}

	return terraformCRIsReady(obj), nil
}

// buildTerraformCR constructs the tofu-controller Terraform CR for a tenant app.
// The CR is placed in tofu-system and points to the generic app-workspace Terraform
// module in the gentian-server GitRepository. Non-sensitive variables (chart ref,
// tenant/app identifiers, extra values) are passed via spec.vars; the Terraform
// module reads all sensitive credentials directly from OpenBao using the vault
// provider with Kubernetes auth.
func buildTerraformCR(
	tenant *gentianov1alpha1.Tenant,
	app gentianov1alpha1.TenantApp,
	profile *gentianov1alpha1.AppProfile,
) (*unstructured.Unstructured, error) {
	nsName := tenantNamespaceName(tenant)
	crName := terraformCRName(tenant.Name, app.Profile)

	// Compute non-sensitive extra values: profile-level ExtraValues merged with
	// per-tenant replica overrides. The Terraform module uses these alongside the
	// sensitive values it reads from OpenBao.
	//
	// Template variable substitution: AppProfiles may reference ${TENANT_DOMAIN}
	// in extraValues for tenant-specific configuration (e.g. Collabora aliasgroups
	// need the Nextcloud host "files.<tenant-domain>"). The operator replaces
	// these placeholders before passing the JSON to the Tofu module.
	extraValuesJSON := ""
	if profile.Spec.ExtraValues != nil && len(profile.Spec.ExtraValues.Raw) > 0 {
		extraValuesJSON = string(profile.Spec.ExtraValues.Raw)
	}
	if extraValuesJSON != "" && tenant.Spec.Domain != "" {
		extraValuesJSON = strings.ReplaceAll(extraValuesJSON, "${TENANT_DOMAIN}", tenant.Spec.Domain)
	}
	if app.Config != nil && app.Config.Replicas != nil {
		extra := map[string]interface{}{}
		if extraValuesJSON != "" {
			if err := json.Unmarshal([]byte(extraValuesJSON), &extra); err != nil {
				return nil, fmt.Errorf("parse ExtraValues for %s: %w", app.Profile, err)
			}
		}
		extra["replicaCount"] = *app.Config.Replicas
		raw, err := json.Marshal(extra)
		if err != nil {
			return nil, fmt.Errorf("marshal extra values for %s: %w", app.Profile, err)
		}
		extraValuesJSON = string(raw)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(terraformGVK)
	obj.SetName(crName)
	obj.SetNamespace(tofuSystemNamespace)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		appLabel:       app.Profile,
		managedByLabel: managedByValue,
	})

	modulePath := tofuModulePath
	if profile.Spec.TofuModulePath != "" {
		modulePath = profile.Spec.TofuModulePath
	}

	_ = unstructured.SetNestedField(obj.Object, true, "spec", "destroyResourcesOnDeletion")
	_ = unstructured.SetNestedField(obj.Object, "auto", "spec", "approvePlan")
	_ = unstructured.SetNestedField(obj.Object, "10m", "spec", "interval")
	_ = unstructured.SetNestedField(obj.Object, modulePath, "spec", "path")
	_ = unstructured.SetNestedField(obj.Object, "GitRepository", "spec", "sourceRef", "kind")
	_ = unstructured.SetNestedField(obj.Object, tofuGitRepositoryName, "spec", "sourceRef", "name")
	// Build the full S3 backend config with a unique key per Terraform CR.
	// customConfiguration is written to backend_override.tf inside a `terraform {}`
	// block by the tofu-controller, and REPLACES (not merges) any backend block in
	// the Terraform module. Therefore all required S3 attributes must be provided.
	backendKey := fmt.Sprintf(
		"backend \"s3\" {\n"+
			"  bucket           = %q\n"+
			"  key              = \"%s/terraform.tfstate\"\n"+
			"  endpoint         = %q\n"+
			"  region           = \"us-east-1\"\n"+
			"  force_path_style = true\n"+
			"  skip_credentials_validation = true\n"+
			"  skip_metadata_api_check     = true\n"+
			"  skip_region_validation      = true\n"+
			"}\n",
		tofuStateBucket, crName, tofuStateEndpoint,
	)
	_ = unstructured.SetNestedField(obj.Object, backendKey, "spec", "backendConfig", "customConfiguration")

	// Non-sensitive variables: chart reference, tenant/app identifiers, and
	// value-mapping keys. The Terraform module uses tenant_name + app_name to
	// derive OpenBao secret paths (gentian-os/tenants/{tenant}/apps/{app}/...),
	// and the vm_* keys to know which Helm values to populate via set_sensitive.
	vars := []interface{}{
		map[string]interface{}{"name": "tenant_name", "value": tenant.Name},
		map[string]interface{}{"name": "app_name", "value": app.Profile},
		map[string]interface{}{"name": "namespace", "value": nsName},
		map[string]interface{}{"name": "chart_repository", "value": profile.Spec.Chart.Repository},
		map[string]interface{}{"name": "chart_name", "value": profile.Spec.Chart.Name},
		map[string]interface{}{"name": "chart_version", "value": profile.Spec.Chart.Version},
	}

	// Append ValueMapping keys so the module knows which Helm values to wire.
	// Empty keys signal "not required" and are omitted to reduce spec noise.
	if vm := profile.Spec.ValueMapping; vm != nil {
		if vm.OIDC != nil {
			if vm.OIDC.IssuerKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_oidc_issuer_key", "value": vm.OIDC.IssuerKey})
			}
			if vm.OIDC.ClientIDKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_oidc_client_id_key", "value": vm.OIDC.ClientIDKey})
			}
			if vm.OIDC.ClientSecretKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_oidc_client_secret_key", "value": vm.OIDC.ClientSecretKey})
			}
		}
		if vm.Database != nil {
			if vm.Database.HostKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_db_host_key", "value": vm.Database.HostKey})
			}
			if vm.Database.PortKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_db_port_key", "value": vm.Database.PortKey})
			}
			if vm.Database.NameKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_db_name_key", "value": vm.Database.NameKey})
			}
			if vm.Database.UserKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_db_user_key", "value": vm.Database.UserKey})
			}
			if vm.Database.PasswordKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_db_password_key", "value": vm.Database.PasswordKey})
			}
		}
		if vm.S3 != nil {
			if vm.S3.EndpointKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_s3_endpoint_key", "value": vm.S3.EndpointKey})
			}
			if vm.S3.BucketKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_s3_bucket_key", "value": vm.S3.BucketKey})
			}
			if vm.S3.AccessKeyKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_s3_access_key_key", "value": vm.S3.AccessKeyKey})
			}
			if vm.S3.SecretKeyKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_s3_secret_key_key", "value": vm.S3.SecretKeyKey})
			}
			if vm.S3.RegionKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_s3_region_key", "value": vm.S3.RegionKey})
			}
		}
		if vm.Cache != nil {
			if vm.Cache.HostKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_cache_host_key", "value": vm.Cache.HostKey})
			}
			if vm.Cache.PortKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_cache_port_key", "value": vm.Cache.PortKey})
			}
			if vm.Cache.PasswordKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_cache_password_key", "value": vm.Cache.PasswordKey})
			}
		}
		if vm.SMTP != nil {
			if vm.SMTP.HostKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_smtp_host_key", "value": vm.SMTP.HostKey})
			}
			if vm.SMTP.PortKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_smtp_port_key", "value": vm.SMTP.PortKey})
			}
			if vm.SMTP.UserKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_smtp_user_key", "value": vm.SMTP.UserKey})
			}
			if vm.SMTP.PasswordKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_smtp_password_key", "value": vm.SMTP.PasswordKey})
			}
		}
		if vm.IMAP != nil {
			if vm.IMAP.HostKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_imap_host_key", "value": vm.IMAP.HostKey})
			}
			if vm.IMAP.PortKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_imap_port_key", "value": vm.IMAP.PortKey})
			}
		}
		if vm.LDAP != nil {
			if vm.LDAP.HostKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_ldap_host_key", "value": vm.LDAP.HostKey})
			}
			if vm.LDAP.PortKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_ldap_port_key", "value": vm.LDAP.PortKey})
			}
			if vm.LDAP.BaseDNKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_ldap_base_dn_key", "value": vm.LDAP.BaseDNKey})
			}
			if vm.LDAP.BindDNKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_ldap_bind_dn_key", "value": vm.LDAP.BindDNKey})
			}
			if vm.LDAP.BindPasswordKey != "" {
				vars = append(vars, map[string]interface{}{"name": "vm_ldap_bind_password_key", "value": vm.LDAP.BindPasswordKey})
			}
		}
	}

	if extraValuesJSON != "" {
		vars = append(vars, map[string]interface{}{"name": "extra_values_json", "value": extraValuesJSON})
	}
	_ = unstructured.SetNestedSlice(obj.Object, vars, "spec", "vars")

	// Registry credentials are sensitive and come from a pre-existing cluster Secret.
	varsFrom := []interface{}{
		map[string]interface{}{"kind": "Secret", "name": "registry-credentials-tofu"},
	}
	_ = unstructured.SetNestedSlice(obj.Object, varsFrom, "spec", "varsFrom")

	// MinIO S3 backend credentials injected as env vars (AWS_ACCESS_KEY_ID etc.).
	envFrom := []interface{}{
		map[string]interface{}{
			"secretRef": map[string]interface{}{"name": "minio-tofu-state"},
		},
	}
	_ = unstructured.SetNestedSlice(obj.Object, envFrom, "spec", "runnerPodTemplate", "spec", "envFrom")

	// Write any Terraform outputs (e.g. app URLs) to a named Secret.
	_ = unstructured.SetNestedField(obj.Object, crName+"-outputs", "spec", "writeOutputsToSecret", "name")

	return obj, nil
}

// terraformCRIsReady returns true when the Terraform CR's Ready condition is True,
// indicating the tofu-controller has successfully applied the workspace.
func terraformCRIsReady(obj *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == "Ready" && cond["status"] == "True" {
			return true
		}
	}
	return false
}

// terraformCRName returns the Terraform CR name for a tenant + app.
func terraformCRName(tenantName, appProfile string) string {
	return fmt.Sprintf("tf-%s-%s", tenantName, appProfile)
}

// buildAppApplication constructs the ArgoCD Application CR for a specific app + tenant.// It renders Helm values from the AppProfile's ValueMapping (kernel-service references)
// and merges extraValues from the profile and per-tenant config overrides.
func buildAppApplication(
	tenant *gentianov1alpha1.Tenant,
	app gentianov1alpha1.TenantApp,
	profile *gentianov1alpha1.AppProfile,
) (*unstructured.Unstructured, error) {
	nsName := tenantNamespaceName(tenant)

	helmValues, err := renderHelmValues(tenant.Name, app.Profile, profile, app.Config)
	if err != nil {
		return nil, err
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(argocdApplicationGVK)
	obj.SetName(appApplicationName(tenant.Name, app.Profile))
	obj.SetNamespace(argocdNamespace)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		appLabel:       app.Profile,
		managedByLabel: managedByValue,
	})
	_ = unstructured.SetNestedField(obj.Object, "default", "spec", "project")
	_ = unstructured.SetNestedField(obj.Object, profile.Spec.Chart.Repository, "spec", "source", "repoURL")
	_ = unstructured.SetNestedField(obj.Object, profile.Spec.Chart.Name, "spec", "source", "chart")
	_ = unstructured.SetNestedField(obj.Object, profile.Spec.Chart.Version, "spec", "source", "targetRevision")
	if helmValues != "" {
		_ = unstructured.SetNestedField(obj.Object, helmValues, "spec", "source", "helm", "values")
	}
	_ = unstructured.SetNestedField(obj.Object, "https://kubernetes.default.svc", "spec", "destination", "server")
	_ = unstructured.SetNestedField(obj.Object, nsName, "spec", "destination", "namespace")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "prune")
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "syncPolicy", "automated", "selfHeal")
	return obj, nil
}

// renderHelmValues builds the YAML string passed to spec.source.helm.values.
//
// Strategy:
//  1. Seed a flat map from the ValueMapping keys with placeholder reference values
//     pointing at where kernel services will inject secrets via ExternalSecret.
//  2. Merge profile-level extraValues (JSON) over the seeded map.
//  3. Merge per-tenant config overrides (replicas) if present.
//
// The resulting map is serialised as YAML-like "key: value\n" lines since ArgoCD
// accepts both YAML and JSON in spec.source.helm.values.
func renderHelmValues(
	tenantName, appName string,
	profile *gentianov1alpha1.AppProfile,
	config *gentianov1alpha1.TenantAppConfig,
) (string, error) {
	values := map[string]interface{}{}

	// 1. Seed from ValueMapping.
	if vm := profile.Spec.ValueMapping; vm != nil {
		if vm.OIDC != nil {
			secretBase := fmt.Sprintf("gentian-os/tenants/%s/apps/%s/oidc", tenantName, appName)
			if vm.OIDC.IssuerKey != "" {
				setNestedDot(values, vm.OIDC.IssuerKey, secretBase+"/issuer")
			}
			if vm.OIDC.ClientIDKey != "" {
				setNestedDot(values, vm.OIDC.ClientIDKey, secretBase+"/client-id")
			}
			if vm.OIDC.ClientSecretKey != "" {
				setNestedDot(values, vm.OIDC.ClientSecretKey, secretBase+"/client-secret")
			}
		}
		if vm.Database != nil {
			secretBase := fmt.Sprintf("gentian-os/tenants/%s/apps/%s/database", tenantName, appName)
			if vm.Database.HostKey != "" {
				setNestedDot(values, vm.Database.HostKey, secretBase+"/host")
			}
			if vm.Database.PortKey != "" {
				setNestedDot(values, vm.Database.PortKey, secretBase+"/port")
			}
			if vm.Database.NameKey != "" {
				setNestedDot(values, vm.Database.NameKey, secretBase+"/name")
			}
			if vm.Database.UserKey != "" {
				setNestedDot(values, vm.Database.UserKey, secretBase+"/user")
			}
			if vm.Database.PasswordKey != "" {
				setNestedDot(values, vm.Database.PasswordKey, secretBase+"/password")
			}
		}
		if vm.S3 != nil {
			secretBase := fmt.Sprintf("gentian-os/tenants/%s/apps/%s/s3", tenantName, appName)
			if vm.S3.EndpointKey != "" {
				setNestedDot(values, vm.S3.EndpointKey, secretBase+"/endpoint")
			}
			if vm.S3.BucketKey != "" {
				setNestedDot(values, vm.S3.BucketKey, secretBase+"/bucket")
			}
			if vm.S3.AccessKeyKey != "" {
				setNestedDot(values, vm.S3.AccessKeyKey, secretBase+"/access-key")
			}
			if vm.S3.SecretKeyKey != "" {
				setNestedDot(values, vm.S3.SecretKeyKey, secretBase+"/secret-key")
			}
			if vm.S3.RegionKey != "" {
				setNestedDot(values, vm.S3.RegionKey, secretBase+"/region")
			}
		}
		if vm.Cache != nil {
			secretBase := fmt.Sprintf("gentian-os/tenants/%s/apps/%s/cache", tenantName, appName)
			if vm.Cache.HostKey != "" {
				setNestedDot(values, vm.Cache.HostKey, secretBase+"/host")
			}
			if vm.Cache.PortKey != "" {
				setNestedDot(values, vm.Cache.PortKey, secretBase+"/port")
			}
			if vm.Cache.PasswordKey != "" {
				setNestedDot(values, vm.Cache.PasswordKey, secretBase+"/password")
			}
		}
		if vm.SMTP != nil {
			secretBase := fmt.Sprintf("gentian-os/tenants/%s/apps/%s/smtp", tenantName, appName)
			if vm.SMTP.HostKey != "" {
				setNestedDot(values, vm.SMTP.HostKey, secretBase+"/host")
			}
			if vm.SMTP.PortKey != "" {
				setNestedDot(values, vm.SMTP.PortKey, secretBase+"/port")
			}
			if vm.SMTP.UserKey != "" {
				setNestedDot(values, vm.SMTP.UserKey, secretBase+"/user")
			}
			if vm.SMTP.PasswordKey != "" {
				setNestedDot(values, vm.SMTP.PasswordKey, secretBase+"/password")
			}
		}
		if vm.IMAP != nil {
			secretBase := fmt.Sprintf("gentian-os/tenants/%s/apps/%s/imap", tenantName, appName)
			if vm.IMAP.HostKey != "" {
				setNestedDot(values, vm.IMAP.HostKey, secretBase+"/host")
			}
			if vm.IMAP.PortKey != "" {
				setNestedDot(values, vm.IMAP.PortKey, secretBase+"/port")
			}
		}
		if vm.LDAP != nil {
			secretBase := fmt.Sprintf("gentian-os/tenants/%s/apps/%s/ldap", tenantName, appName)
			if vm.LDAP.HostKey != "" {
				setNestedDot(values, vm.LDAP.HostKey, secretBase+"/host")
			}
			if vm.LDAP.PortKey != "" {
				setNestedDot(values, vm.LDAP.PortKey, secretBase+"/port")
			}
			if vm.LDAP.BaseDNKey != "" {
				setNestedDot(values, vm.LDAP.BaseDNKey, secretBase+"/base-dn")
			}
			if vm.LDAP.BindDNKey != "" {
				setNestedDot(values, vm.LDAP.BindDNKey, secretBase+"/bind-dn")
			}
			if vm.LDAP.BindPasswordKey != "" {
				setNestedDot(values, vm.LDAP.BindPasswordKey, secretBase+"/bind-password")
			}
		}
	}

	// 2. Merge profile-level extraValues over the seeded map.
	if profile.Spec.ExtraValues != nil && len(profile.Spec.ExtraValues.Raw) > 0 {
		extra := map[string]interface{}{}
		if err := json.Unmarshal(profile.Spec.ExtraValues.Raw, &extra); err != nil {
			return "", fmt.Errorf("parse AppProfile extraValues: %w", err)
		}
		mergeMaps(values, extra)
	}

	// 3. Apply per-tenant config overrides.
	if config != nil && config.Replicas != nil {
		values["replicaCount"] = *config.Replicas
	}

	if len(values) == 0 {
		return "", nil
	}

	return marshalValues(values), nil
}

// setNestedDot sets a value in a nested map using dot-notation key paths.
// Bracket-notation paths (e.g. key["sub"]) are stored as a flat key to avoid
// mis-parsing; ArgoCD passes them verbatim to Helm.
func setNestedDot(m map[string]interface{}, key, value string) {
	// Keys containing bracket notation are stored as a flat entry; Helm interprets them.
	if strings.ContainsAny(key, "[]") {
		m[key] = value
		return
	}
	parts := strings.SplitN(key, ".", 2)
	if len(parts) == 1 {
		m[key] = value
		return
	}
	sub, ok := m[parts[0]].(map[string]interface{})
	if !ok {
		sub = map[string]interface{}{}
		m[parts[0]] = sub
	}
	setNestedDot(sub, parts[1], value)
}

// mergeMaps merges src into dst, overwriting existing keys at each level.
func mergeMaps(dst, src map[string]interface{}) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]interface{}); ok {
			if dstMap, ok := dst[k].(map[string]interface{}); ok {
				mergeMaps(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}

// marshalValues serialises a nested map as simple "key: value" YAML lines.
// This naive format is sufficient for string scalars and scalar integers, which
// is all that ValueMapping and TenantAppConfig produce. ExtraValues that contain
// nested structures are marshalled as JSON (handled by mergeMaps before this call).
func marshalValues(m map[string]interface{}) string {
	raw, err := json.Marshal(m)
	if err != nil {
		// Should never happen with string/int values.
		return ""
	}
	// Return JSON — ArgoCD's helm.values field accepts both YAML and JSON.
	return string(raw)
}

// appApplicationName returns the ArgoCD Application CR name for a tenant + app.
func appApplicationName(tenantName, appProfile string) string {
	return fmt.Sprintf("app-%s-%s", tenantName, appProfile)
}
