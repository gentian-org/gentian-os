/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The acceptance of a contract between a placed and an installed component,
// as the tenant's reconciler does it against an API server: the profiles pass
// the CRD's validation, the placed provider's binding carries its endpoint,
// and the grant from the Tenant opens both contract policies -- selecting
// the provider's pods by the instance label of the release the operator
// installs and the installed consumer's by the app label it is rendered with,
// on the contract's port only. Withdrawing the grant from the
// Tenant closes them again. The vault and ExternalSecret halves are in the
// fake-client test of the same name (component_integrations_test.go): this
// suite runs without a vault.
func TestAContractBetweenAPlacedAndAnInstalledComponentOpensItsPathOnTheAPIServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	provider := newProviderProfile("placed-desk", "desk-api")
	provider.Spec.TrustTier = gentianov1alpha1.TrustTierPlatform
	provider.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementEvery}
	provider.Spec.Provides[0].Protocol = "http"
	provider.Spec.Provides[0].Endpoint = &gentianov1alpha1.ContractEndpoint{Service: "placed-desk-api", Port: 8000}
	if err := testClient.Create(ctx, provider); err != nil {
		t.Fatalf("create provider ComponentProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), provider) })

	consumer := newConsumerProfile("placed-store", "desk-api", "placed-desk")
	consumer.Spec.Integrations[0].Capabilities = []string{"tiles"}
	consumer.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{Integrations: map[string]gentianov1alpha1.IntegrationValueMapping{
		"desk-api": {EndpointKey: "api.env.DESKTOP_API_URL"},
	}}
	if err := testClient.Create(ctx, consumer); err != nil {
		t.Fatalf("create consumer ComponentProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), consumer) })

	tenantName := "placed-contract"
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = tenantName
	tenant.Spec = gentianov1alpha1.TenantSpec{
		DisplayName: "Placed Contract Co",
		Apps:        []gentianov1alpha1.TenantApp{{Profile: "placed-desk", Placed: true}, {Profile: "placed-store"}},
		Grants: []gentianov1alpha1.TenantAppGrant{{AppGrantSpec: gentianov1alpha1.AppGrantSpec{
			App: "placed-store", Consume: []gentianov1alpha1.ConsumeGrantSpec{{Contract: "desk-api", Granted: []string{"tiles"}}},
		}}},
	}
	if err := testClient.Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	ns := "tenant-" + tenantName
	bindingName := tenantName + "--placed-store--desk-api"
	ib := &gentianov1alpha1.IntegrationBinding{}
	waitFor(t, tenantReadyTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: bindingName, Namespace: ns}, ib) == nil
	})
	if e := ib.Spec.Endpoint; e == nil || e.Service != "placed-desk-api" || e.Port != 8000 {
		t.Fatalf("binding endpoint = %+v", ib.Spec.Endpoint)
	}

	// The placed desk is selected by its release, the installed store by
	// the app label its pods are rendered with.
	instance, app := "app.kubernetes.io/instance", "gentianos.io/app"
	desk := ns + "-placed-desk"
	store := "placed-store"
	out, in := &networkingv1.NetworkPolicy{}, &networkingv1.NetworkPolicy{}
	waitFor(t, tenantReadyTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "contract-" + bindingName, Namespace: ns}, out) == nil &&
			testClient.Get(ctx, types.NamespacedName{Name: "contract-in-" + bindingName, Namespace: ns}, in) == nil
	})
	if out.Spec.PodSelector.MatchLabels[app] != store || len(out.Spec.Egress) != 1 ||
		out.Spec.Egress[0].To[0].PodSelector.MatchLabels[instance] != desk ||
		len(out.Spec.Egress[0].Ports) != 1 || out.Spec.Egress[0].Ports[0].Port.IntVal != 8000 {
		t.Fatalf("consumer policy = %+v", out.Spec)
	}
	if in.Spec.PodSelector.MatchLabels[instance] != desk || len(in.Spec.Ingress) != 1 ||
		in.Spec.Ingress[0].From[0].PodSelector.MatchLabels[app] != store ||
		len(in.Spec.Ingress[0].Ports) != 1 || in.Spec.Ingress[0].Ports[0].Port.IntVal != 8000 {
		t.Fatalf("provider policy = %+v", in.Spec)
	}

	// Withdrawn in the Tenant: both policies go.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &gentianov1alpha1.Tenant{}
		if err := testClient.Get(ctx, types.NamespacedName{Name: tenantName}, current); err != nil {
			return err
		}
		current.Spec.Grants = nil
		return testClient.Update(ctx, current)
	}); err != nil {
		t.Fatalf("withdraw the grant: %v", err)
	}
	waitFor(t, tenantReadyTimeout, func() bool {
		errOut := testClient.Get(ctx, types.NamespacedName{Name: "contract-" + bindingName, Namespace: ns}, &networkingv1.NetworkPolicy{})
		errIn := testClient.Get(ctx, types.NamespacedName{Name: "contract-in-" + bindingName, Namespace: ns}, &networkingv1.NetworkPolicy{})
		return apierrors.IsNotFound(errOut) && apierrors.IsNotFound(errIn)
	})
}
