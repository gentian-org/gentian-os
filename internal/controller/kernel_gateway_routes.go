// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	kernelRouteKeycloakIDP      = "kernel-idp"
	kernelRoutePortal           = "kernel-portal"
	kernelRouteCryptpad         = "kernel-cryptpad"
	kernelRouteCryptpadSandbox  = "kernel-cryptpad-sandbox"
)

type kernelHTTPRouteSpec struct {
	name        string
	host        string
	serviceName string
	servicePort int32
	pathPrefix  string
	filters     []gatewayv1.HTTPRouteFilter
	policy      map[string]interface{}
}

func kernelStage() string {
	return envOrDefault("GENTIAN_STAGE", envOrDefault("ENV", "dev"))
}

func keycloakProxyServiceName() string {
	if v := envOrDefault("KEYCLOAK_PROXY_SERVICE", ""); v != "" {
		return v
	}
	return fmt.Sprintf("nubus-%s-keycloak-extensions-proxy", kernelStage())
}

func portalFrontendServiceName() string {
	if v := envOrDefault("PORTAL_FRONTEND_SERVICE", ""); v != "" {
		return v
	}
	return fmt.Sprintf("nubus-%s-portal-frontend", kernelStage())
}

func (r *GatewayPlatformReconciler) reconcileKernelHTTPRoutes(ctx context.Context) error {
	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for kernel HTTPRoutes: %w", err)
	}

	var effectiveDomains []string
	var tenantNames []string
	for i := range tenantList.Items {
		if tenantList.Items[i].DeletionTimestamp != nil {
			continue
		}
		if d := tenantList.Items[i].EffectiveDomain(r.KernelDomain, r.TenancyMode); d != "" {
			effectiveDomains = append(effectiveDomains, d)
			tenantNames = append(tenantNames, tenantList.Items[i].Name)
		}
	}
	oidcSubs, err := collectOIDCIngressSubdomainsByTenant(ctx, r.Client, tenantList.Items)
	if err != nil {
		return fmt.Errorf("collect OIDC ingress subdomains: %w", err)
	}

	specs := kernelHTTPRouteSpecs(r.KernelDomain, effectiveDomains, oidcSubs, tenantNames)
	expected := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		expected[spec.name] = struct{}{}
		route := buildKernelHTTPRoute(spec)
		if err := ensureHTTPRouteResource(ctx, r.Client, route); err != nil {
			return fmt.Errorf("ensure kernel HTTPRoute %s: %w", spec.name, err)
		}
		if spec.policy != nil {
			if err := r.ensureKernelBackendTrafficPolicy(ctx, spec); err != nil {
				return fmt.Errorf("ensure kernel BackendTrafficPolicy %s: %w", spec.name, err)
			}
		}
	}
	return r.deleteStaleKernelHTTPRoutes(ctx, expected)
}

func kernelHTTPRouteSpecs(
	kernelDomain string,
	tenantEffectiveDomains []string,
	tenantOIDCSubdomains map[string][]string,
	tenantNames []string,
) []kernelHTTPRouteSpec {
	idHost := fmt.Sprintf("id.%s", kernelDomain)
	portalHost := kernelPortalHost(kernelDomain)
	padHost := fmt.Sprintf("pad.%s", kernelDomain)
	padSandboxHost := fmt.Sprintf("pad-sandbox.%s", kernelDomain)

	return []kernelHTTPRouteSpec{
		{
			name:        kernelRouteKeycloakIDP,
			host:        idHost,
			serviceName: keycloakProxyServiceName(),
			servicePort: 8080,
			pathPrefix:  "/",
			filters:     keycloakGatewayResponseFilters(kernelDomain, tenantEffectiveDomains, tenantOIDCSubdomains, tenantNames),
			policy:      keycloakProxyBackendTrafficPolicySpec(),
		},
		{
			name:        kernelRoutePortal,
			host:        portalHost,
			serviceName: portalFrontendServiceName(),
			servicePort: 80,
			pathPrefix:  "/",
		},
		{
			name:        kernelRouteCryptpad,
			host:        padHost,
			serviceName: "cryptpad",
			servicePort: 80,
			pathPrefix:  "/",
			filters:     kernelCryptpadMainResponseFilters(kernelDomain),
			policy:      cryptpadBackendTrafficPolicySpec(),
		},
		{
			name:        kernelRouteCryptpadSandbox,
			host:        padSandboxHost,
			serviceName: "cryptpad",
			servicePort: 80,
			pathPrefix:  "/",
			filters:     kernelCryptpadSandboxResponseFilters(kernelDomain),
			policy:      cryptpadBackendTrafficPolicySpec(),
		},
	}
}

func keycloakProxyBackendTrafficPolicySpec() map[string]interface{} {
	return map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
			},
		},
		"connection": map[string]interface{}{
			"bufferLimit": "128k",
		},
	}
}

func cryptpadBackendTrafficPolicySpec() map[string]interface{} {
	return map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
			},
		},
		"timeout": map[string]interface{}{
			"http": map[string]interface{}{
				"requestTimeout":  "3600s",
				"responseTimeout": "3600s",
			},
		},
	}
}

func buildKernelHTTPRoute(spec kernelHTTPRouteSpec) *gatewayv1.HTTPRoute {
	port := gatewayv1.PortNumber(spec.servicePort)
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch(spec.pathPrefix)},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(spec.serviceName),
						Port: &port,
					},
				},
			},
		},
	}
	if len(spec.filters) > 0 {
		rule.Filters = spec.filters
	}

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.name,
			Namespace: servicesNamespace,
			Labels: map[string]string{
				managedByLabel:        managedByValue,
				gatewayComponentLabel: gatewayComponentKernel,
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					gatewayParentRef(KernelPublicGatewayName),
				},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(spec.host)},
			Rules:     []gatewayv1.HTTPRouteRule{rule},
		},
	}
}

func (r *GatewayPlatformReconciler) ensureKernelBackendTrafficPolicy(ctx context.Context, spec kernelHTTPRouteSpec) error {
	if spec.policy == nil {
		return nil
	}
	policySpec := cloneMap(spec.policy)
	attachBackendTrafficPolicyTarget(policySpec, spec.name)

	name := fmt.Sprintf("btp-%s", spec.name)
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(backendTrafficPolicyGVK)
	desired.SetName(name)
	desired.SetNamespace(servicesNamespace)
	desired.SetLabels(map[string]string{
		managedByLabel:        managedByValue,
		gatewayComponentLabel: gatewayComponentKernel,
	})
	if err := unstructured.SetNestedField(desired.Object, policySpec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(backendTrafficPolicyGVK)
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: servicesNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, policySpec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func cloneMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *GatewayPlatformReconciler) deleteStaleKernelHTTPRoutes(ctx context.Context, expected map[string]struct{}) error {
	list := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, list,
		client.InNamespace(servicesNamespace),
		client.MatchingLabels{managedByLabel: managedByValue, gatewayComponentLabel: gatewayComponentKernel},
	); err != nil {
		return err
	}
	for i := range list.Items {
		if _, ok := expected[list.Items[i].Name]; ok {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func kernelKeycloakHTTPRouteName() string {
	return kernelRouteKeycloakIDP
}
