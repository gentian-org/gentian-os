// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// newProviderProfile builds an AppProfile that provides the given contract.
func newProviderProfile(name, contract string) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      name,
			DeploymentMethod: gentianov1alpha1.DeploymentMethodArgoCD,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       name,
				Version:    "0.1.0",
			},
			Provides: []gentianov1alpha1.ContractRef{
				{Name: contract},
			},
		},
	}
}

// newConsumerProfile builds an AppProfile that optionally integrates with the given contract.
func newConsumerProfile(name, contract, provider string) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      name,
			DeploymentMethod: gentianov1alpha1.DeploymentMethodArgoCD,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       name,
				Version:    "0.1.0",
			},
			OptionalIntegrations: []gentianov1alpha1.IntegrationRef{
				{Contract: contract, Provider: provider},
			},
		},
	}
}

// TestBindings_NoIntegrations: no app has OptionalIntegrations → BindingsReady=True, NoBindingsRequired.
func TestBindings_NoIntegrations(t *testing.T) {
	t.Parallel()
	profile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "bind-profile-none"},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      "bind-profile-none",
			DeploymentMethod: gentianov1alpha1.DeploymentMethodArgoCD,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       "bind-profile-none",
				Version:    "0.1.0",
			},
		},
	}
	if err := testClient.Create(context.Background(), profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "bind-none"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Bind None Co",
			Domain:      "bind-none.example.com",
			AdminEmail:  "admin@bind-none.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "bind-profile-none"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 15*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "bind-none"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	var cond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "BindingsReady" {
			cond = &updated.Status.Conditions[i]
			break
		}
	}
	if cond == nil {
		t.Fatal("expected BindingsReady condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected BindingsReady=True, got %v", cond.Status)
	}
	if cond.Reason != "NoBindingsRequired" {
		t.Errorf("expected reason NoBindingsRequired, got %q", cond.Reason)
	}
}

// TestBindings_ProviderPresent: consumer + provider both in tenant → IntegrationBinding created.
func TestBindings_ProviderPresent(t *testing.T) {
	t.Parallel()
	providerProfile := newProviderProfile("bind-provider-app", "file-store")
	if err := testClient.Create(context.Background(), providerProfile); err != nil {
		t.Fatalf("create provider AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), providerProfile) })

	consumerProfile := newConsumerProfile("bind-consumer-app", "file-store", "bind-provider-app")
	if err := testClient.Create(context.Background(), consumerProfile); err != nil {
		t.Fatalf("create consumer AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), consumerProfile) })

	tenantName := "bind-both"
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Bind Both Co",
			Domain:      "bind-both.example.com",
			AdminEmail:  "admin@bind-both.example.com",
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "bind-provider-app"},
				{Profile: "bind-consumer-app"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	nsName := fmt.Sprintf("tenant-%s", tenantName)
	ibName := fmt.Sprintf("%s--bind-consumer-app--file-store", tenantName)

	ib := &gentianov1alpha1.IntegrationBinding{}
	waitFor(t, 15*time.Second, func() bool {
		err := testClient.Get(context.Background(), types.NamespacedName{Name: ibName, Namespace: nsName}, ib)
		return err == nil
	})

	if ib.Spec.Contract != "file-store" {
		t.Errorf("expected contract file-store, got %q", ib.Spec.Contract)
	}
	if ib.Spec.Provider.App != "bind-provider-app" {
		t.Errorf("expected provider bind-provider-app, got %q", ib.Spec.Provider.App)
	}
	if ib.Spec.Consumer.App != "bind-consumer-app" {
		t.Errorf("expected consumer bind-consumer-app, got %q", ib.Spec.Consumer.App)
	}
	if ib.Spec.Provider.Namespace != nsName {
		t.Errorf("expected provider namespace %q, got %q", nsName, ib.Spec.Provider.Namespace)
	}
}

// TestBindings_ProviderAbsent: consumer in tenant but provider not present → no IntegrationBinding.
func TestBindings_ProviderAbsent(t *testing.T) {
	t.Parallel()
	consumerProfile := newConsumerProfile("bind-consumer-only", "file-store", "bind-provider-missing")
	if err := testClient.Create(context.Background(), consumerProfile); err != nil {
		t.Fatalf("create consumer AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), consumerProfile) })

	tenantName := "bind-no-provider"
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "No Provider Co",
			Domain:      "bind-no-provider.example.com",
			AdminEmail:  "admin@bind-no-provider.example.com",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "bind-consumer-only"}},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 15*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: tenantName}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	nsName := fmt.Sprintf("tenant-%s", tenantName)
	ibName := fmt.Sprintf("%s--bind-consumer-only--file-store", tenantName)
	ib := &gentianov1alpha1.IntegrationBinding{}
	err := testClient.Get(context.Background(), types.NamespacedName{Name: ibName, Namespace: nsName}, ib)
	if err == nil {
		t.Error("expected no IntegrationBinding when provider is absent, but one exists")
	}
	// Verify BindingsReady=True (no bindings required — provider absent)
	var cond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "BindingsReady" {
			cond = &updated.Status.Conditions[i]
			break
		}
	}
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("expected BindingsReady=True, cond=%v", cond)
	}
}

// TestBindings_GarbageCollectOnProviderRemoval: delete provider → stale IntegrationBinding removed.
func TestBindings_GarbageCollectOnProviderRemoval(t *testing.T) {
	t.Parallel()
	providerProfile := newProviderProfile("bind-gc-provider", "file-store")
	if err := testClient.Create(context.Background(), providerProfile); err != nil {
		t.Fatalf("create provider AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), providerProfile) })

	consumerProfile := newConsumerProfile("bind-gc-consumer", "file-store", "bind-gc-provider")
	if err := testClient.Create(context.Background(), consumerProfile); err != nil {
		t.Fatalf("create consumer AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), consumerProfile) })

	tenantName := "bind-gc"
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "GC Bind Co",
			Domain:      "bind-gc.example.com",
			AdminEmail:  "admin@bind-gc.example.com",
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "bind-gc-provider"},
				{Profile: "bind-gc-consumer"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	nsName := fmt.Sprintf("tenant-%s", tenantName)
	ibName := fmt.Sprintf("%s--bind-gc-consumer--file-store", tenantName)

	// Wait for IntegrationBinding to be created.
	ib := &gentianov1alpha1.IntegrationBinding{}
	waitFor(t, 15*time.Second, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: ibName, Namespace: nsName}, ib) == nil
	})

	// Remove the provider app from the tenant. Retry on conflict because the
	// controller may reconcile the tenant (bumping resourceVersion) between our
	// Get and Update calls.
	for {
		updated := &gentianov1alpha1.Tenant{}
		if err := testClient.Get(context.Background(), types.NamespacedName{Name: tenantName}, updated); err != nil {
			t.Fatalf("get tenant: %v", err)
		}
		updated.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "bind-gc-consumer"}}
		err := testClient.Update(context.Background(), updated)
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) {
			t.Fatalf("update tenant apps: %v", err)
		}
		// conflict — controller touched the object; re-fetch and retry
	}

	// IntegrationBinding should be garbage-collected.
	waitFor(t, 15*time.Second, func() bool {
		err := testClient.Get(context.Background(), types.NamespacedName{Name: ibName, Namespace: nsName}, &gentianov1alpha1.IntegrationBinding{})
		return err != nil
	})
}
