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

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	conditionBindingsReady = "BindingsReady"
	bindingsRequeueAfter   = 2 * time.Second
)

// ensureIntegrationBindings waits for Crossplane-provisioned IntegrationBinding
// CRs and garbage-collects stale bindings.
func (r *TenantReconciler) ensureIntegrationBindings(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	nsName := tenantNamespaceName(tenant)

	desiredList, err := r.collectDesiredIntegrationBindings(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	desired := make(map[string]*gentianov1alpha1.IntegrationBinding, len(desiredList))
	for _, ib := range desiredList {
		desired[ib.Name] = ib
	}

	if len(desired) == 0 {
		r.setCondition(tenant, conditionBindingsReady, metav1.ConditionTrue,
			"NoBindingsRequired", "No integration bindings are required for this tenant")
		return ctrl.Result{}, r.gcStaleIntegrationBindings(ctx, nsName, desired)
	}

	allReady := true
	for _, ib := range desired {
		existing := &gentianov1alpha1.IntegrationBinding{}
		err := r.Get(ctx, types.NamespacedName{Name: ib.Name, Namespace: ib.Namespace}, existing)
		if errors.IsNotFound(err) {
			allReady = false
			continue
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("get IntegrationBinding %s: %w", ib.Name, err)
		}
	}

	if err := r.gcStaleIntegrationBindings(ctx, nsName, desired); err != nil {
		return ctrl.Result{}, err
	}

	if !allReady {
		r.setCondition(tenant, conditionBindingsReady, metav1.ConditionFalse,
			"Provisioning", "Waiting for integration bindings to be provisioned")
		return ctrl.Result{RequeueAfter: bindingsRequeueAfter}, nil
	}

	r.setCondition(tenant, conditionBindingsReady, metav1.ConditionTrue,
		"Provisioned", "All integration bindings are provisioned")
	return ctrl.Result{}, nil
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
			Auth: &gentianov1alpha1.BindingAuth{
				Method:    "api-key",
				VaultPath: secrets.ContractPath(tenantName, integration.Contract),
			},
		},
	}
}

// --- Name helpers -----------------------------------------------------------

// integrationBindingName returns a deterministic name for an IntegrationBinding.
// Format: {tenantName}--{consumerApp}--{contract}
func integrationBindingName(tenantName, consumerApp, contract string) string {
	return fmt.Sprintf("%s--%s--%s", tenantName, consumerApp, contract)
}
