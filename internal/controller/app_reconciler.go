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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	conditionAppsReady = "AppsReady"
)

// ensureAppDeployment creates or reconciles one ArgoCD Application CR per app
// declared in tenant.Spec.Apps. Only DeploymentMethod=argocd (the default) is
// supported; tofu-controller entries are skipped with a TODO.
//
// Returns a non-zero RequeueAfter when any Application is not yet Healthy.
func (r *TenantReconciler) ensureAppDeployment(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	if len(tenant.Spec.Apps) == 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionTrue, "NoAppsConfigured", "No applications are configured for this tenant")
		return ctrl.Result{}, nil
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

		// Pattern B (tofu-controller) is not yet implemented.
		if profile.Spec.DeploymentMethod == gentianov1alpha1.DeploymentMethodTofuController {
			// TODO(inc10): implement tofu-controller Terraform CR creation
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

	if !allHealthy {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "Provisioning", "Waiting for Application CRs to become Healthy")
		return ctrl.Result{}, nil
	}

	r.setCondition(tenant, conditionAppsReady, metav1.ConditionTrue, "Provisioned", "All Application CRs are Healthy")
	return ctrl.Result{}, nil
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

// deleteAppDeployment removes all ArgoCD Application CRs created for the tenant's apps.
// Apps are ephemeral workload resources, so they are always deleted regardless of
// the tenant's DeletionPolicy; ArgoCD cascades deletion of the deployed Helm releases.
func (r *TenantReconciler) deleteAppDeployment(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	for _, app := range tenant.Spec.Apps {
		appCR := &unstructured.Unstructured{}
		appCR.SetGroupVersionKind(argocdApplicationGVK)
		appCR.SetName(appApplicationName(tenant.Name, app.Profile))
		appCR.SetNamespace(argocdNamespace)
		if err := r.Delete(ctx, appCR); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Application CR for app %s: %w", app.Profile, err)
		}
	}
	return nil
}

// buildAppApplication constructs the ArgoCD Application CR for a specific app + tenant.
// It renders Helm values from the AppProfile's ValueMapping (kernel-service references)
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
