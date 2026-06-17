/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the License.
*/

package controller

import (
	"context"
	"fmt"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// buildTenantEdgeObjects returns Gateway API edge resources owned by Crossplane:
// Certificate, Gateway, HTTPRoutes, ReferenceGrants, and BackendTrafficPolicy.
// Cloudflare DNS and stale-resource cleanup stay in ensureGateway.
func (r *TenantReconciler) buildTenantEdgeObjects(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]client.Object, error) {
	if !isGatewayRoutingMode(r.RoutingMode) {
		return nil, nil
	}

	intents, err := r.collectTenantIngressIntents(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if len(intents) == 0 {
		return nil, nil
	}

	effectiveDomain := r.tenantEffectiveDomain(tenant)
	if effectiveDomain == "" {
		return nil, fmt.Errorf("tenant %q has no effective domain for gateway manifests", tenant.Name)
	}

	nsName := tenantNamespaceName(tenant)
	wildcardCertName := tenantWildcardCertName(tenant.Name)
	tlsSecret := tenantWildcardSecretName(tenant.Name)

	var objects []client.Object
	objects = append(objects,
		buildTenantWildcardCertificate(tenant, nsName, wildcardCertName, tlsSecret, effectiveDomain, r.tenantDNS01ClusterIssuer()),
	)
	gw := buildTenantGateway(tenant, nsName, effectiveDomain, tlsSecret)
	gw.SetGroupVersionKind(gatewayv1.SchemeGroupVersion.WithKind("Gateway"))
	objects = append(objects, gw)
	objects = append(objects, buildTenantReferenceGrantObjects(tenant)...)

	for _, intent := range intents {
		host := ingressHost(intent.appProfile, intent.ingress, effectiveDomain)
		route := buildAppHTTPRoute(tenant, nsName, intent.appProfile, intent.ingress, host, effectiveDomain, r.KernelDomain)
		route.SetGroupVersionKind(gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"))
		objects = append(objects, route)
		if btp := buildAppBackendTrafficPolicyObject(tenant, nsName, intent.appProfile, intent.ingress); btp != nil {
			objects = append(objects, btp)
		}
	}

	return objects, nil
}

// waitForTenantEdgeResources reports whether Crossplane-provisioned edge resources
// are programmed for the tenant.
func (r *TenantReconciler) waitForTenantEdgeResources(ctx context.Context, tenant *gentianov1alpha1.Tenant) (bool, string, error) {
	if !isGatewayRoutingMode(r.RoutingMode) {
		return true, "", nil
	}

	intents, err := r.collectTenantIngressIntents(ctx, tenant)
	if err != nil {
		return false, "", err
	}
	if len(intents) == 0 {
		return true, "", nil
	}

	nsName := tenantNamespaceName(tenant)
	effectiveDomain := r.tenantEffectiveDomain(tenant)
	if effectiveDomain == "" {
		return false, "NoDomain", nil
	}

	tlsSecret := tenantWildcardSecretName(tenant.Name)
	desiredGW := buildTenantGateway(tenant, nsName, effectiveDomain, tlsSecret)
	if programmed, reason := gatewayProgrammed(ctx, r.Client, desiredGW); !programmed {
		return false, reason, nil
	}

	for _, intent := range intents {
		host := ingressHost(intent.appProfile, intent.ingress, effectiveDomain)
		route := buildAppHTTPRoute(tenant, nsName, intent.appProfile, intent.ingress, host, effectiveDomain, r.KernelDomain)
		if ok, reason := httpRouteProgrammed(ctx, r.Client, route); !ok {
			return false, reason, nil
		}
	}
	return true, "", nil
}
