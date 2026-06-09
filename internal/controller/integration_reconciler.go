// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	conditionBindingsReady = "BindingsReady"
)

// ensureIntegrationBindings creates or reconciles IntegrationBinding CRs for a
// tenant. For every app whose AppProfile declares an OptionalIntegration, the
// reconciler checks whether the named provider app is also in the tenant's app
// list. When both sides are present, an IntegrationBinding is created in the
// tenant namespace and kept in sync. Stale bindings (provider removed from the
// tenant) are garbage-collected via a label scan.
func (r *TenantReconciler) ensureIntegrationBindings(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	nsName := tenantNamespaceName(tenant)

	// Build a fast lookup set of app profile names present in the tenant.
	presentApps := make(map[string]struct{}, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		presentApps[app.Profile] = struct{}{}
	}

	// Collect a map of desired bindings: bindingName -> desired IntegrationBinding.
	desired := map[string]*gentianov1alpha1.IntegrationBinding{}

	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		for _, integration := range profile.Spec.OptionalIntegrations {
			// Determine the provider app: use explicit Provider field if set,
			// otherwise match any app in the tenant that declares this contract.
			providerApp := ""
			if integration.Provider != "" {
				if _, ok := presentApps[integration.Provider]; ok {
					providerApp = integration.Provider
				}
			} else {
				// Scan other apps in the tenant for a matching Provides contract.
				providerApp = r.findProviderInTenant(ctx, tenant, integration.Contract, presentApps, app.Profile)
			}
			if providerApp == "" {
				continue // provider not present; skip
			}
			name := integrationBindingName(tenant.Name, app.Profile, integration.Contract)
			ib := buildIntegrationBinding(name, nsName, tenant.Name, app.Profile, providerApp, integration)
			desired[name] = ib
		}
	}

	if len(desired) == 0 {
		r.setCondition(tenant, conditionBindingsReady, metav1.ConditionTrue,
			"NoBindingsRequired", "No integration bindings are required for this tenant")
		return ctrl.Result{}, r.gcStaleIntegrationBindings(ctx, nsName, desired)
	}

	// Create or update each desired binding.
	for _, ib := range desired {
		if err := r.ensureIntegrationBinding(ctx, ib); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure IntegrationBinding %s: %w", ib.Name, err)
		}
	}

	// Garbage-collect bindings that are no longer desired.
	if err := r.gcStaleIntegrationBindings(ctx, nsName, desired); err != nil {
		return ctrl.Result{}, err
	}

	r.setCondition(tenant, conditionBindingsReady, metav1.ConditionTrue,
		"Provisioned", "All integration bindings are provisioned")
	return ctrl.Result{}, nil
}

// ensureIntegrationBinding creates or updates a single IntegrationBinding CR.
func (r *TenantReconciler) ensureIntegrationBinding(ctx context.Context, desired *gentianov1alpha1.IntegrationBinding) error {
	existing := &gentianov1alpha1.IntegrationBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		existing.Spec = desired.Spec
		return r.Update(ctx, existing)
	}
	return nil
}

// gcStaleIntegrationBindings deletes IntegrationBinding CRs in the tenant namespace
// that are no longer in the desired set (i.e., provider was removed from the tenant).
func (r *TenantReconciler) gcStaleIntegrationBindings(
	ctx context.Context,
	nsName string,
	desired map[string]*gentianov1alpha1.IntegrationBinding,
) error {
	existing := &gentianov1alpha1.IntegrationBindingList{}
	if err := r.List(ctx, existing,
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue},
	); err != nil {
		return fmt.Errorf("list IntegrationBindings in %s: %w", nsName, err)
	}
	for i := range existing.Items {
		ib := &existing.Items[i]
		if _, keep := desired[ib.Name]; !keep {
			if err := r.Delete(ctx, ib); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("delete stale IntegrationBinding %s: %w", ib.Name, err)
			}
		}
	}
	return nil
}

// deleteIntegrationBindings removes all managed IntegrationBinding CRs from the
// tenant namespace. Called unconditionally on tenant deletion regardless of
// DeletionPolicy.
func (r *TenantReconciler) deleteIntegrationBindings(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	nsName := tenantNamespaceName(tenant)
	existing := &gentianov1alpha1.IntegrationBindingList{}
	if err := r.List(ctx, existing,
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue},
	); err != nil {
		return fmt.Errorf("list IntegrationBindings for deletion in %s: %w", nsName, err)
	}
	for i := range existing.Items {
		ib := &existing.Items[i]
		if err := r.Delete(ctx, ib); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete IntegrationBinding %s: %w", ib.Name, err)
		}
	}
	return nil
}

// findProviderInTenant scans the tenant's app list for an app whose AppProfile
// declares the given contract in its Provides list. Returns the provider app name
// or an empty string if none is found. The consumer app is excluded from the search.
func (r *TenantReconciler) findProviderInTenant(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	contract string,
	presentApps map[string]struct{},
	consumerApp string,
) string {
	for appName := range presentApps {
		if appName == consumerApp {
			continue
		}
		providerProfile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: appName}, providerProfile); err != nil {
			continue
		}
		for _, c := range providerProfile.Spec.Provides {
			if c.Name == contract {
				return appName
			}
		}
	}
	return ""
}

// --- Builder ----------------------------------------------------------------

// buildIntegrationBinding constructs an IntegrationBinding CR.
func buildIntegrationBinding(
	name, nsName, tenantName, consumerApp, providerApp string,
	integration gentianov1alpha1.IntegrationRef,
) *gentianov1alpha1.IntegrationBinding {
	return &gentianov1alpha1.IntegrationBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenantName,
				appLabel:       consumerApp,
				managedByLabel: managedByValue,
			},
		},
		Spec: gentianov1alpha1.IntegrationBindingSpec{
			Contract: integration.Contract,
			Provider: gentianov1alpha1.AppEndpoint{
				App:       providerApp,
				Namespace: nsName,
			},
			Consumer: gentianov1alpha1.AppEndpoint{
				App:       consumerApp,
				Namespace: nsName,
			},
			Capabilities: integration.Capabilities,
		},
	}
}

// --- Name helpers -----------------------------------------------------------

// integrationBindingName returns a deterministic name for an IntegrationBinding.
// Format: {tenantName}--{consumerApp}--{contract}
func integrationBindingName(tenantName, consumerApp, contract string) string {
	return fmt.Sprintf("%s--%s--%s", tenantName, consumerApp, contract)
}
