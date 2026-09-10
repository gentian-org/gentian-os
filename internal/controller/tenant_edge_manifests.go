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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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
	// Tenant app hostnames are served by the kernel Gateway. The tenant's
	// certificate stays in the tenant namespace and is referenced across
	// namespaces by the ReferenceGrants below, so certificate ownership follows
	// the tenant without a Gateway of its own.
	objects = append(objects, buildTenantReferenceGrantObjects(tenant)...)

	for _, route := range appHTTPRoutesForIntents(tenant, nsName, intents, effectiveDomain, r.KernelDomain) {
		route.SetGroupVersionKind(gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"))
		objects = append(objects, route)
	}
	for _, intent := range intents {
		if btp := buildAppBackendTrafficPolicyObject(tenant, nsName, intent.appProfile, intent.ingress); btp != nil {
			objects = append(objects, btp)
		}
	}
	// The escaped-slashes ClientTrafficPolicy for this tenant's listener is
	// created alongside the kernel Gateway, since an Envoy Gateway policy must
	// live in the same namespace as the Gateway it targets.

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

	// Edge readiness is the kernel Gateway's: it terminates TLS and carries the
	// tenant listener for this tenant's subdomains.
	kernelGW := &gatewayv1.Gateway{}
	kernelGW.Name = KernelPublicGatewayName
	kernelGW.Namespace = servicesNamespace
	if programmed, reason := gatewayProgrammed(ctx, r.Client, kernelGW); !programmed {
		return false, reason, nil
	}

	for _, route := range appHTTPRoutesForIntents(tenant, nsName, intents, effectiveDomain, r.KernelDomain) {
		if ok, reason := httpRouteProgrammed(ctx, r.Client, route); !ok {
			return false, reason, nil
		}
	}
	return true, "", nil
}
