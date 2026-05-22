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
	"fmt"

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
)

// appClaimGVK is the GVK for the namespace-scoped App claim created by the
// App XRD. The operator creates one claim per (tenant, app-profile) pair in
// the tenant's namespace. Crossplane reconciles the claim into a Release and
// ExternalSecret via the App Composition.
var appClaimGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "App",
}

// ensureAppDeployment creates or reconciles one App claim per app declared in
// tenant.Spec.Apps, and cleans up claims for apps removed from the spec.
//
// Returns a non-zero RequeueAfter when any claim is not yet Ready.
func (r *TenantReconciler) ensureAppDeployment(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	// desiredApps tracks the set of App claims that should exist. Shared-mode
	// apps (consumers of the primary tenant's deployment) are excluded because
	// they use no tenant-side deployment. Primary-mode apps (the hosting tenant)
	// DO create an App claim and are included.
	desiredApps := make(map[string]struct{}, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		if app.IsolationMode != gentianov1alpha1.AppDeploymentModeShared {
			desiredApps[app.Profile] = struct{}{}
		}
	}

	if len(tenant.Spec.Apps) == 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionTrue, "NoAppsConfigured", "No applications are configured for this tenant")
	}

	allReady := true

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

		// Shared-mode apps (consumers of the primary tenant's deployment) use
		// no tenant-side Helm release. The identity reconciler handles shared-apps
		// IAM brokering; the primary tenant (AppDeploymentModePrimary) owns the
		// actual deployment and continues through to ensureAppClaim below.
		if app.IsolationMode == gentianov1alpha1.AppDeploymentModeShared {
			continue
		}

		// Seed app-internal secrets into OpenBao before the Composition reads
		// them. SeedAppSecret is idempotent; repeated calls are safe.
		if err := r.seedAppSecrets(ctx, tenant, app.Profile, profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("seed app-secrets for %s: %w", app.Profile, err)
		}

		ready, err := r.ensureAppClaim(ctx, tenant, app, profile)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure App claim for %s: %w", app.Profile, err)
		}
		if !ready {
			allReady = false
		}
	}

	if err := r.cleanupOrphanedAppCRs(ctx, tenant, desiredApps); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup orphaned App claims: %w", err)
	}

	if len(tenant.Spec.Apps) > 0 && !allReady {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionFalse, "Provisioning", "Waiting for App claims to become Ready")
		return ctrl.Result{}, nil
	}

	if len(tenant.Spec.Apps) > 0 {
		r.setCondition(tenant, conditionAppsReady, metav1.ConditionTrue, "Provisioned", "All App claims are Ready")
	}
	return ctrl.Result{}, nil
}

// seedAppSecrets writes each AppProfile.spec.appSecrets entry into OpenBao at
// …/internal/{name} with key "value". No-op when Seeder is nil or the profile
// declares no app-secrets. Repeated calls are idempotent.
func (r *TenantReconciler) seedAppSecrets(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string, profile *gentianov1alpha1.AppProfile) error {
	if r.Seeder == nil || len(profile.Spec.AppSecrets) == 0 {
		return nil
	}
	for _, s := range profile.Spec.AppSecrets {
		if s.Name == "" {
			continue
		}
		if _, err := r.Seeder.SeedAppSecret(ctx, tenant.Name, appName, s.Name); err != nil {
			return err
		}
	}
	return nil
}

// cleanupOrphanedAppCRs lists all App claims managed by this operator for the
// given tenant, and deletes any whose app label is not in desiredApps. Crossplane
// cascades deletion to the composed ExternalSecret and Release via ownerRefs.
func (r *TenantReconciler) cleanupOrphanedAppCRs(ctx context.Context, tenant *gentianov1alpha1.Tenant, desiredApps map[string]struct{}) error {
	labelSelector := client.MatchingLabels{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	}

	claimList := &unstructured.UnstructuredList{}
	claimList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   appClaimGVK.Group,
		Version: appClaimGVK.Version,
		Kind:    appClaimGVK.Kind + "List",
	})
	nsName := tenantNamespaceName(tenant)
	if err := r.List(ctx, claimList, client.InNamespace(nsName), labelSelector); err != nil {
		return fmt.Errorf("list App claims: %w", err)
	}
	for i := range claimList.Items {
		appName := claimList.Items[i].GetLabels()[appLabel]
		if appName == "" {
			continue
		}
		if _, desired := desiredApps[appName]; !desired {
			if err := r.Delete(ctx, &claimList.Items[i]); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("delete orphaned App claim %s: %w", claimList.Items[i].GetName(), err)
			}
		}
	}
	return nil
}

// ensureAppClaim creates (or checks readiness of) the App claim for a single
// app within a tenant. Returns true when the claim's Ready condition is True.
func (r *TenantReconciler) ensureAppClaim(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	app gentianov1alpha1.TenantApp,
	profile *gentianov1alpha1.AppProfile,
) (bool, error) {
	claimName := app.Profile
	nsName := tenantNamespaceName(tenant)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(appClaimGVK)
	err := r.Get(ctx, types.NamespacedName{Name: claimName, Namespace: nsName}, obj)
	if errors.IsNotFound(err) {
		desired := buildAppClaim(tenant, app, r.KernelDomain, profile)
		return false, r.Create(ctx, desired)
	}
	if err != nil {
		return false, err
	}
	return appClaimIsReady(obj), nil
}

// buildAppClaim constructs the App claim for a tenant app. The claim is placed
// in the tenant namespace so tenant-admin RBAC applies. Crossplane reconciles
// the claim through the App Composition which creates an ExternalSecret and a
// provider-helm Release in the same namespace.
func buildAppClaim(
	tenant *gentianov1alpha1.Tenant,
	app gentianov1alpha1.TenantApp,
	kernelDomain string,
	profile *gentianov1alpha1.AppProfile,
) *unstructured.Unstructured {
	nsName := tenantNamespaceName(tenant)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(appClaimGVK)
	obj.SetName(app.Profile)
	obj.SetNamespace(nsName)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		appLabel:       app.Profile,
		managedByLabel: managedByValue,
	})

	_ = unstructured.SetNestedField(obj.Object, app.Profile, "spec", "profileRef", "name")
	_ = unstructured.SetNestedField(obj.Object, nsName, "spec", "tenantNamespace")

	if profile != nil && profile.Spec.CompositionRef != "" {
		_ = unstructured.SetNestedField(obj.Object, profile.Spec.CompositionRef, "spec", "compositionRef", "name")
	}

	if domain := tenant.EffectiveDomain(kernelDomain); domain != "" {
		_ = unstructured.SetNestedField(obj.Object, domain, "spec", "domain")
	}

	if app.Config != nil {
		if app.Config.Replicas != nil {
			_ = unstructured.SetNestedField(obj.Object, int64(*app.Config.Replicas), "spec", "config", "replicas")
		}
	}

	return obj
}

// deleteAppDeployment removes all App claims created for the tenant's apps.
// Crossplane cascades deletion to the composed ExternalSecret and Release via
// ownerReferences, so no manual cleanup of those resources is needed.
func (r *TenantReconciler) deleteAppDeployment(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	labelSelector := client.MatchingLabels{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	}

	nsName := tenantNamespaceName(tenant)
	claimList := &unstructured.UnstructuredList{}
	claimList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   appClaimGVK.Group,
		Version: appClaimGVK.Version,
		Kind:    appClaimGVK.Kind + "List",
	})
	if err := r.List(ctx, claimList, client.InNamespace(nsName), labelSelector); err != nil {
		return fmt.Errorf("list App claims for tenant %s: %w", tenant.Name, err)
	}
	for i := range claimList.Items {
		if err := r.Delete(ctx, &claimList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete App claim %s: %w", claimList.Items[i].GetName(), err)
		}
	}
	return nil
}

// appClaimIsReady returns true when the App claim's Ready condition is True,
// indicating Crossplane has fully reconciled the ExternalSecret and Release.
func appClaimIsReady(obj *unstructured.Unstructured) bool {
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
