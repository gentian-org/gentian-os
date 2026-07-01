// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	kernelRouteKeycloakIDP   = "kernel-idp"
	kernelRouteKernelApex    = "kernel-apex-redirect"
	kernelRouteArgoCD        = "kernel-argocd"
	kernelRouteGentianPortal = "kernel-gentian-portal"

	gentianPortalAPIService = "gentian-portal-gentian-portal-api"
	gentianPortalWebService = "gentian-portal-gentian-portal-web"

	argocdServerServiceName = "argocd-server"
)

type kernelHTTPRouteSpec struct {
	name         string
	host         string
	rules        []gatewayv1.HTTPRouteRule
	policy       map[string]interface{}
	clientPolicy map[string]interface{}
}

// suzeKeycloakHTTPServiceName is the keycloakx chart HTTP Service for Stage 1 Suze IdP.
func suzeKeycloakHTTPServiceName() string {
	if v := envOrDefault("KEYCLOAK_HTTP_SERVICE", ""); v != "" {
		return v
	}
	return "gentian-idp-keycloak-keycloakx-http"
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
	if err := r.ensureArgoCDReferenceGrant(ctx); err != nil {
		return fmt.Errorf("ensure ArgoCD ReferenceGrant: %w", err)
	}
	if err := r.ensureGentianPortalReferenceGrant(ctx); err != nil {
		return fmt.Errorf("ensure Gentian portal ReferenceGrant: %w", err)
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
		if spec.clientPolicy != nil {
			if err := r.ensureKernelClientTrafficPolicy(ctx, spec); err != nil {
				return fmt.Errorf("ensure kernel ClientTrafficPolicy %s: %w", spec.name, err)
			}
		}
	}
	needsWildcard, err := clusterNeedsEscapedSlashesKeepUnchanged(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("detect escaped-slashes gateway policy need: %w", err)
	}
	if needsWildcard {
		if err := r.ensureKernelClientTrafficPolicyNamed(ctx, "kernel-wildcard-escaped-slashes", "https-wildcard", escapedSlashesKeepUnchangedClientTrafficPolicySpec()); err != nil {
			return fmt.Errorf("ensure kernel wildcard escaped-slashes ClientTrafficPolicy: %w", err)
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

	kcService := suzeKeycloakHTTPServiceName()
	kcPort := int32(8080)

	specs := []kernelHTTPRouteSpec{
		{
			name: kernelRouteKeycloakIDP,
			host: idHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS(
					kcService,
					kernelNamespace,
					kcPort,
					"/",
					keycloakGatewayResponseFilters(kernelDomain, tenantEffectiveDomains, tenantOIDCSubdomains, tenantNames)...,
				),
			},
			policy: keycloakProxyBackendTrafficPolicySpec(),
		},
	}
	// Gentian UI portal (API + SPA) runs in platform-kernel; edge traffic reaches
	// kernel-public-gateway in servicesNamespace via Cloudflare tunnel.
	specs = append(specs, kernelHTTPRouteSpec{
		name:  kernelRouteGentianPortal,
		host:  portalHost,
		rules: kernelGentianPortalHTTPRouteRules(),
	})
	specs = append(specs,
		kernelHTTPRouteSpec{
			name: kernelRouteKernelApex,
			host: kernelDomain,
			rules: []gatewayv1.HTTPRouteRule{
				kernelApexRedirectRule(kernelDomain),
			},
		},
		kernelHTTPRouteSpec{
			name: kernelRouteArgoCD,
			host: fmt.Sprintf("argocd.%s", kernelDomain),
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRuleCrossNamespace(argocdServerServiceName, argocdNamespace, 80),
			},
		},
	)
	return specs
}

func kernelGentianPortalHTTPRouteRules() []gatewayv1.HTTPRouteRule {
	return []gatewayv1.HTTPRouteRule{
		kernelBackendRulePrefixNS(gentianPortalAPIService, kernelNamespace, 8000, "/api"),
		kernelBackendRuleExactNS(gentianPortalAPIService, kernelNamespace, 8000, "/healthz"),
		kernelBackendRuleExactNS(gentianPortalAPIService, kernelNamespace, 8000, "/readyz"),
		kernelBackendRulePrefixNS(gentianPortalWebService, kernelNamespace, 8080, "/"),
	}
}

func ptrPortNumber(port int32) *gatewayv1.PortNumber {
	p := gatewayv1.PortNumber(port)
	return &p
}

func kernelBackendRule(serviceName string, port int32, filters []gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return kernelBackendRulePrefix(serviceName, port, "/", filters...)
}

func kernelBackendRulePrefix(serviceName string, port int32, prefix string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return kernelBackendRulePrefixNS(serviceName, "", port, prefix, filters...)
}

func kernelBackendRulePrefixNS(serviceName, namespace string, port int32, prefix string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	p := gatewayv1.PortNumber(port)
	ref := gatewayv1.BackendObjectReference{
		Name: gatewayv1.ObjectName(serviceName),
		Port: &p,
	}
	if namespace != "" {
		ns := gatewayv1.Namespace(namespace)
		ref.Namespace = &ns
	}
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch(prefix)},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: ref}},
		},
	}
	if len(filters) > 0 {
		rule.Filters = filters
	}
	return rule
}

func kernelBackendRuleExact(serviceName string, port int32, path string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return kernelBackendRuleExactNS(serviceName, "", port, path, filters...)
}

func kernelBackendRuleExactNS(serviceName, namespace string, port int32, path string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	p := gatewayv1.PortNumber(port)
	ref := gatewayv1.BackendObjectReference{
		Name: gatewayv1.ObjectName(serviceName),
		Port: &p,
	}
	if namespace != "" {
		ns := gatewayv1.Namespace(namespace)
		ref.Namespace = &ns
	}
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathExactMatch(path)},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: ref}},
		},
	}
	if len(filters) > 0 {
		rule.Filters = filters
	}
	return rule
}

func kernelBackendRuleCrossNamespace(serviceName, namespace string, port int32) gatewayv1.HTTPRouteRule {
	p := gatewayv1.PortNumber(port)
	ns := gatewayv1.Namespace(namespace)
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name:      gatewayv1.ObjectName(serviceName),
						Namespace: &ns,
						Port:      &p,
					},
				},
			},
		},
	}
}

func (r *GatewayPlatformReconciler) ensureArgoCDReferenceGrant(ctx context.Context) error {
	spec := map[string]interface{}{
		"from": []interface{}{
			map[string]interface{}{
				"group":     gatewayv1.GroupName,
				"kind":      "HTTPRoute",
				"namespace": servicesNamespace,
			},
		},
		"to": []interface{}{
			map[string]interface{}{
				"group": "",
				"kind":  "Service",
			},
		},
	}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(referenceGrantGVK)
	desired.SetName("allow-kernel-gateway-routes")
	desired.SetNamespace(argocdNamespace)
	desired.SetLabels(map[string]string{
		managedByLabel: managedByValue,
	})
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(referenceGrantGVK)
	err := r.Get(ctx, client.ObjectKey{Name: desired.GetName(), Namespace: argocdNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func (r *GatewayPlatformReconciler) ensureGentianPortalReferenceGrant(ctx context.Context) error {
	spec := map[string]interface{}{
		"from": []interface{}{
			map[string]interface{}{
				"group":     gatewayv1.GroupName,
				"kind":      "HTTPRoute",
				"namespace": servicesNamespace,
			},
		},
		"to": []interface{}{
			map[string]interface{}{
				"group": "",
				"kind":  "Service",
			},
		},
	}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(referenceGrantGVK)
	desired.SetName("allow-kernel-gateway-routes")
	desired.SetNamespace(kernelNamespace)
	desired.SetLabels(map[string]string{
		managedByLabel: managedByValue,
	})
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(referenceGrantGVK)
	err := r.Get(ctx, client.ObjectKey{Name: desired.GetName(), Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func kernelRedirectRule(path, target string, status int) gatewayv1.HTTPRouteRule {
	pathType := gatewayv1.FullPathHTTPPathModifier
	targetPath := target
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathExactMatch(path)},
		Filters: []gatewayv1.HTTPRouteFilter{
			{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Path: &gatewayv1.HTTPPathModifier{
						Type:            pathType,
						ReplaceFullPath: &targetPath,
					},
					StatusCode: &status,
				},
			},
		},
	}
}

func kernelApexRedirectRule(kernelDomain string) gatewayv1.HTTPRouteRule {
	scheme := "https"
	status := 302
	port := gatewayv1.PortNumber(443)
	pathType := gatewayv1.FullPathHTTPPathModifier
	loginPath := "/login/"
	portalHost := gatewayv1.PreciseHostname(kernelPortalHost(kernelDomain))
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
		Filters: []gatewayv1.HTTPRouteFilter{
			{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Scheme:     &scheme,
					Hostname:   &portalHost,
					Path:       &gatewayv1.HTTPPathModifier{Type: pathType, ReplaceFullPath: &loginPath},
					Port:       &port,
					StatusCode: &status,
				},
			},
		},
	}
}

func pathExactMatch(path string) gatewayv1.HTTPRouteMatch {
	t := gatewayv1.PathMatchExact
	return gatewayv1.HTTPRouteMatch{
		Path: &gatewayv1.HTTPPathMatch{
			Type:  &t,
			Value: &path,
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

func escapedSlashesKeepUnchangedClientTrafficPolicySpec() map[string]interface{} {
	return map[string]interface{}{
		"path": map[string]interface{}{
			// WOPI/WebSocket URLs may embed encoded paths (%3A, %2F, …).
			// Envoy default normalization rejects them with path_normalization_failed.
			"escapedSlashesAction": "KeepUnchanged",
		},
	}
}

func buildKernelHTTPRoute(spec kernelHTTPRouteSpec) *gatewayv1.HTTPRoute {
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
			Rules:     spec.rules,
		},
	}
}

var clientTrafficPolicyGVK = schema.GroupVersionKind{
	Group:   "gateway.envoyproxy.io",
	Version: "v1alpha1",
	Kind:    "ClientTrafficPolicy",
}

func (r *GatewayPlatformReconciler) ensureKernelClientTrafficPolicy(ctx context.Context, spec kernelHTTPRouteSpec) error {
	if spec.clientPolicy == nil {
		return nil
	}
	return r.ensureKernelClientTrafficPolicyNamed(ctx, spec.name, "https-wildcard", spec.clientPolicy)
}

func (r *GatewayPlatformReconciler) ensureKernelClientTrafficPolicyNamed(
	ctx context.Context,
	policyName, sectionName string,
	clientPolicy map[string]interface{},
) error {
	policySpec := cloneMap(clientPolicy)
	attachKernelClientTrafficPolicyTarget(policySpec, sectionName)

	name := fmt.Sprintf("ctp-%s", policyName)
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(clientTrafficPolicyGVK)
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
	existing.SetGroupVersionKind(clientTrafficPolicyGVK)
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

func attachKernelClientTrafficPolicyTarget(spec map[string]interface{}, sectionName string) {
	spec["targetRefs"] = []interface{}{
		map[string]interface{}{
			"group":       gatewayv1.GroupName,
			"kind":        "Gateway",
			"name":        KernelPublicGatewayName,
			"sectionName": sectionName,
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

func cloneMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
