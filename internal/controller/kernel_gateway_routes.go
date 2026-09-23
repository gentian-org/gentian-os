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

	corev1 "k8s.io/api/core/v1"
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
	kernelRouteKeycloakIDP      = "kernel-idp"
	kernelRouteKernelApex       = "kernel-apex-redirect"
	kernelRouteHTTPRedirect     = "kernel-http-redirect"
	kernelRouteArgoCD           = "kernel-argocd"
	kernelRouteHeadlamp         = "kernel-headlamp"
	kernelRouteGentianPortal    = "kernel-gentian-portal"
	kernelRouteGentianPortalWWW = "kernel-gentian-portal-www"
	kernelRouteLiteLLM          = "kernel-llm"

	gentianPortalAPIService = "gentian-portal-gentian-portal-api"
	gentianPortalWebService = "gentian-portal-gentian-portal-web"

	argocdServerServiceName = "argocd-server"
	headlampServiceName     = "headlamp"
	litellmProxyServiceName = "litellm-proxy"
	litellmProxyPort        = int32(4000)
)

type kernelHTTPRouteSpec struct {
	name string
	// host empty means "match every hostname" — used by the :80 redirect, which
	// must catch apex, wildcard and tenant domains without enumerating them.
	host  string
	rules []gatewayv1.HTTPRouteRule
	// sectionName binds the route to a single Gateway listener. Without it a
	// route attaches to every listener whose hostname matches, which for the
	// HTTP->HTTPS redirect would include the :443 listeners and send TLS
	// requests into an infinite redirect back to themselves.
	sectionName  string
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
	// A route lives beside the Gateway; the Services it points at live where
	// their own function does, so each of those namespaces grants the
	// reference. Duplicates in the v4 layout, where they are one namespace.
	for _, ns := range dedupe(argocdNamespace, identityNamespace, servicesNamespace, observabilityNamespace) {
		if err := r.ensureRouteReferenceGrant(ctx, ns); err != nil {
			return fmt.Errorf("ensure ReferenceGrant in %s: %w", ns, err)
		}
	}

	specs := kernelHTTPRouteSpecs(r.KernelDomain, effectiveDomains, oidcSubs, tenantNames,
		clusterLLMEnabled(ctx, r.Client), portalDeployed(ctx, r.Client))
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
		if err := r.ensureKernelClientTrafficPolicyNamed(ctx, "kernel-wildcard-escaped-slashes", wildcardListenerName, escapedSlashesKeepUnchangedClientTrafficPolicySpec()); err != nil {
			return fmt.Errorf("ensure kernel wildcard escaped-slashes ClientTrafficPolicy: %w", err)
		}
		for _, tenantName := range tenantNames {
			name := fmt.Sprintf("tenant-%s-wildcard-escaped-slashes", tenantName)
			sectionName := tenantGatewayListenerName(tenantName)
			if err := r.ensureKernelClientTrafficPolicyNamed(ctx, name, sectionName, escapedSlashesKeepUnchangedClientTrafficPolicySpec()); err != nil {
				return fmt.Errorf("ensure kernel tenant wildcard escaped-slashes ClientTrafficPolicy: %w", err)
			}
		}
	}
	return r.deleteStaleKernelHTTPRoutes(ctx, expected)
}

// portalDeployed reports whether the Gentian portal is actually running, which
// is what its routes wait for.
//
// Read from the Service rather than from a claim field: the claim says what the
// cluster should have, and a hostname published ahead of the workload is a
// public 500 for however long the gap lasts. The web Service is the one the
// SPA is served from, so it is the one whose absence means "not yet".
func portalDeployed(ctx context.Context, c client.Reader) bool {
	svc := &corev1.Service{}
	err := c.Get(ctx, client.ObjectKey{Name: gentianPortalWebService, Namespace: servicesNamespace}, svc)
	return err == nil
}

func kernelHTTPRouteSpecs(
	kernelDomain string,
	tenantEffectiveDomains []string,
	tenantOIDCSubdomains map[string][]string,
	tenantNames []string,
	llmEnabled bool,
	portalDeployed bool,
) []kernelHTTPRouteSpec {
	idHost := fmt.Sprintf("id.%s", kernelDomain)
	portalHost := kernelPortalHost(kernelDomain)

	kcService := suzeKeycloakHTTPServiceName()
	kcPort := int32(8080)

	specs := []kernelHTTPRouteSpec{
		{
			name:        kernelRouteKeycloakIDP,
			host:        idHost,
			sectionName: wildcardListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS(
					kcService,
					identityNamespace,
					kcPort,
					"/",
					keycloakGatewayResponseFilters(kernelDomain, tenantEffectiveDomains, tenantOIDCSubdomains, tenantNames)...,
				),
			},
			policy: keycloakProxyBackendTrafficPolicySpec(),
		},
	}
	// The Gentian UI portal (API + SPA); edge traffic reaches
	// kernel-public-gateway in servicesNamespace via the tunnel.
	//
	// Only once the portal is actually deployed. A route whose backends do not
	// exist is not inert: the hostname is published on the tunnel and given a
	// DNS record, so the portal's address becomes a public 500 rather than a
	// name that does not resolve yet.
	if portalDeployed {
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteGentianPortal,
			host:        portalHost,
			sectionName: wildcardListenerName,
			rules:       kernelGentianPortalHTTPRouteRules(),
		})
		// The same desktop on the name people type into a browser. Served
		// rather than redirected: a redirect would change the address bar
		// mid-login and drop anything travelling on the request, and the
		// portal is one deployment either way.
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteGentianPortalWWW,
			host:        "www." + kernelDomain,
			sectionName: wildcardListenerName,
			rules:       kernelGentianPortalHTTPRouteRules(),
		})
	}
	// Serve the portal on each tenant's own host, rather than redirecting there to
	// the shared one.
	//
	// A redirect cannot carry anything: the gateway filter replaces path and query
	// wholesale, so a login hint travelling on <tenant>.<kernel-domain> was dropped
	// before the portal ever saw it. Answering directly also means the address bar
	// stays on the tenant's name instead of bouncing through portal.<kernel-domain>.
	//
	// Same rules and the same backends as the shared route, so there is one portal
	// deployment answering on more names — not a copy per tenant, which would put
	// the portal's Keycloak admin credentials inside every tenant's blast radius.
	//
	// Consequence worth knowing: tokens live in sessionStorage, which is per origin,
	// so a user signed in on the tenant host is a separate session from the same
	// user on portal.<kernel-domain>. Keycloak's SSO cookie makes crossing between
	// them silent, but they are two sessions.
	for i, domain := range tenantEffectiveDomains {
		if i >= len(tenantNames) {
			break
		}
		if !portalDeployed {
			break
		}
		specs = append(specs, kernelHTTPRouteSpec{
			name: fmt.Sprintf("tenant-%s-portal", tenantNames[i]),
			host: domain,
			// The tenant apex listener carries the tenant's own certificate.
			// buildKernelGateway creates it from the same filtered tenant list
			// that produced this route, so it is always present.
			sectionName: wildcardListenerName,
			rules:       kernelGentianPortalHTTPRouteRules(),
		})
	}
	// The apex sends visitors to the portal, so it waits for the same thing the
	// portal route does rather than redirecting to a name that does not resolve.
	if portalDeployed {
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteKernelApex,
			host:        kernelDomain,
			sectionName: wildcardListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelApexRedirectRule(kernelDomain),
			},
		})
	}
	specs = append(specs,
		kernelHTTPRouteSpec{
			// Plaintext :80 -> https, bound to the http-redirect listener only.
			name:        kernelRouteHTTPRedirect,
			sectionName: httpRedirectListenerName,
			rules:       []gatewayv1.HTTPRouteRule{kernelHTTPSRedirectRule()},
		},
		kernelHTTPRouteSpec{
			name:        kernelRouteArgoCD,
			host:        fmt.Sprintf("argocd.%s", kernelDomain),
			sectionName: wildcardListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRuleCrossNamespace(argocdServerServiceName, argocdNamespace, 80),
			},
		},
	)
	// The cluster view. It belongs with the other kernel consoles rather than
	// beside its own Deployment: a route the operator owns is the one that gets
	// a tunnel hostname and a DNS record, and one created elsewhere is
	// reachable from inside the cluster and nowhere else.
	//
	// Only where the layout has an observability namespace. A v4 cluster has
	// none and runs no Headlamp, and a route to a Service that does not exist
	// would still claim the hostname on the tunnel.
	if observabilityNamespace != "" {
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteHeadlamp,
			host:        fmt.Sprintf("headlamp.%s", kernelDomain),
			sectionName: wildcardListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRuleCrossNamespace(headlampServiceName, observabilityNamespace, 80),
			},
		})
	}
	// LiteLLM admin console — platform-level only (the claim's llm.enabled).
	// Tenants do not get their own route; app-catalogue "litellm" tiles stay
	// unused until per-tenant access is designed (see docs/design/llms.md).
	if llmEnabled {
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteLiteLLM,
			host:        fmt.Sprintf("llm.%s", kernelDomain),
			sectionName: wildcardListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS(litellmProxyServiceName, servicesNamespace, litellmProxyPort, "/"),
			},
		})
	}
	return specs
}

func kernelGentianPortalHTTPRouteRules() []gatewayv1.HTTPRouteRule {
	return []gatewayv1.HTTPRouteRule{
		kernelBackendRulePrefixNS(gentianPortalAPIService, servicesNamespace, 8000, "/api"),
		kernelBackendRuleExactNS(gentianPortalAPIService, servicesNamespace, 8000, "/healthz"),
		kernelBackendRuleExactNS(gentianPortalAPIService, servicesNamespace, 8000, "/readyz"),
		kernelBackendRulePrefixNS(gentianPortalWebService, servicesNamespace, 8080, "/"),
	}
}

// kernelBackendRuleNS routes one match to one Service, optionally cross-namespace.
//
// The prefix and exact variants below were full copies of this, differing in
// which path matcher they called.
func kernelBackendRuleNS(serviceName, namespace string, port int32, match gatewayv1.HTTPRouteMatch, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
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
		Matches: []gatewayv1.HTTPRouteMatch{match},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: ref}},
		},
	}
	if len(filters) > 0 {
		rule.Filters = filters
	}
	return rule
}

func kernelBackendRulePrefixNS(serviceName, namespace string, port int32, prefix string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return kernelBackendRuleNS(serviceName, namespace, port, pathPrefixMatch(prefix), filters...)
}

func kernelBackendRuleExactNS(serviceName, namespace string, port int32, path string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return kernelBackendRuleNS(serviceName, namespace, port, pathExactMatch(path), filters...)
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

// ensureRouteReferenceGrant lets the kernel gateway's HTTPRoutes, which live in
// the services namespace, reference Services in ns.
//
// This was two functions — one for argocd, one for platform-kernel — identical
// for forty lines apart from the namespace they targeted. Which is exactly the
// shape that goes wrong quietly: a change to the grant made in one and not the
// other leaves half the kernel's routes unable to resolve their backend, and the
// symptom is a 500 from the gateway rather than anything naming a ReferenceGrant.
func (r *GatewayPlatformReconciler) ensureRouteReferenceGrant(ctx context.Context, ns string) error {
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
	desired.SetNamespace(ns)
	desired.SetLabels(map[string]string{
		managedByLabel: managedByValue,
	})
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(referenceGrantGVK)
	err := r.Get(ctx, client.ObjectKey{Name: desired.GetName(), Namespace: ns}, existing)
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

// kernelHTTPSRedirectRule sends any plaintext request straight to https on the
// same host and path.
//
// 301 (permanent) is the industry-standard status for http->https: it is
// cacheable, and it is what HSTS preload and every scanner expects. Gateway API
// permits only 301 or 302 here, so 308 — which would additionally preserve the
// request method — is not available; that is immaterial in practice because the
// requests reaching :80 are browsers issuing GET on a bare hostname.
//
// Hostname is deliberately not set, so the requested host is preserved and one
// rule covers apex, wildcard and tenant domains.
func kernelHTTPSRedirectRule() gatewayv1.HTTPRouteRule {
	scheme := "https"
	status := 301
	port := gatewayv1.PortNumber(443)
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
		Filters: []gatewayv1.HTTPRouteFilter{
			{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Scheme:     &scheme,
					Port:       &port,
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
	// No trailing slash. The portal's router declares the route as "/login"
	// (frontend/src/router.tsx) and TanStack Router does not normalise the
	// difference — "/login/" matches nothing and renders its not-found page. The
	// static server answers both with 200 and index.html, so this is invisible
	// from the outside: only the browser sees the 404, and only via the apex
	// redirect, since nothing in the app ever links to "/login/".
	loginPath := "/login"
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
	parentRef := gatewayParentRef(KernelPublicGatewayName)
	if spec.sectionName != "" {
		s := gatewayv1.SectionName(spec.sectionName)
		parentRef.SectionName = &s
	}

	// An empty Hostnames list matches every host, which is what the :80
	// redirect needs. Emitting []Hostname{""} instead would be rejected.
	var hostnames []gatewayv1.Hostname
	if spec.host != "" {
		hostnames = []gatewayv1.Hostname{gatewayv1.Hostname(spec.host)}
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
					parentRef,
				},
			},
			Hostnames: hostnames,
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
	return r.ensureKernelClientTrafficPolicyNamed(ctx, spec.name, wildcardListenerName, spec.clientPolicy)
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

// dedupe returns the distinct namespaces, in order. Several functions share
// one namespace in the v4 layout and a grant applied twice is a conflict.
func dedupe(ns ...string) []string {
	seen := make(map[string]bool, len(ns))
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
