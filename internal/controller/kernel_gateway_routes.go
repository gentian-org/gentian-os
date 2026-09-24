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
	kernelRouteKeycloakAdmin = "kernel-id-admin"
	// kernelRouteKeycloakRefused carries the paths id.<kernel> refuses -- the
	// master realm and the admin console -- as a route of its own, more
	// specific than the allow and closed by a policy: what a perimeter
	// surface refuses is written down, not left to absence.
	kernelRouteKeycloakRefused = "kernel-idp-refused"
	kernelRouteKernelApex      = "kernel-apex-redirect"
	kernelRouteHTTPRedirect    = "kernel-http-redirect"
	kernelRouteArgoCD          = "kernel-argocd"
	kernelRouteHeadlamp        = "kernel-headlamp"
	// The name people type: an alias of the console, by redirect.
	kernelRouteWWWRedirect = "kernel-www-redirect"
	kernelRouteLiteLLM     = "kernel-llm"

	// consoleSubdomain is the desktop's host label in every zone
	// (networking.md §3): console.<kernel> for the platform, console.<t>.<kernel>
	// for a tenant. The desktop profile exposes it under this name, and
	// every redirect and frame policy here assumes it.
	consoleSubdomain = "console"

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
	sectionName string
	// gateway names the edge the route attaches to; empty is the
	// authenticated Gateway. Only the identity provider's realm endpoints
	// and the :80 redirect are the perimeter's.
	gateway      string
	policy       map[string]interface{}
	clientPolicy map[string]interface{}
	// securityPolicy is a SecurityPolicy of the route's own, for a route with
	// no zone session: the refusal on the identity provider's perimeter.
	securityPolicy map[string]interface{}
	// authz is the L2 question for a route behind the kernel zone's session;
	// nil for a route with no session (the perimeter's) or none yet.
	authz *routeAuthz
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
		clusterLLMEnabled(ctx, r.Client), r.Cluster, r.kernelZoneReady(ctx), desktopPresent(ctx, r.Client))
	// The shim's table first: a route whose policy asks the shim before the
	// shim knows the host is refused, which is the right direction, but a
	// short one.
	if err := r.ensureEdgeAuthzRouteTable(ctx, specs); err != nil {
		return fmt.Errorf("ensure edge-authz route table: %w", err)
	}
	expected := make(map[string]struct{}, len(specs))
	expectedPolicies := map[string]struct{}{}
	for _, spec := range specs {
		expected[spec.name] = struct{}{}
		route := buildKernelHTTPRoute(spec)
		if err := ensureHTTPRouteResource(ctx, r.Client, route); err != nil {
			return fmt.Errorf("ensure kernel HTTPRoute %s: %w", spec.name, err)
		}
		if spec.authz != nil || spec.securityPolicy != nil {
			expectedPolicies[kernelSecurityPolicyName(spec.name)] = struct{}{}
			if err := r.ensureKernelSecurityPolicy(ctx, spec); err != nil {
				return fmt.Errorf("ensure kernel SecurityPolicy %s: %w", spec.name, err)
			}
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
	if err := r.deleteStaleKernelSecurityPolicies(ctx, expectedPolicies); err != nil {
		return fmt.Errorf("delete stale kernel SecurityPolicies: %w", err)
	}
	return r.deleteStaleKernelHTTPRoutes(ctx, expected)
}

// desktopPresent reports whether this cluster ships a desktop at all: the
// profile the operator chart installs (ui-restructure.md §1). Without it
// there is no console anywhere, and nothing is redirected to one.
func desktopPresent(ctx context.Context, c client.Reader) bool {
	profile := &gentianov1alpha1.ComponentProfile{}
	return c.Get(ctx, client.ObjectKey{Name: DesktopProfileName}, profile) == nil
}

// consoleHost is where a zone's desktop answers, console.<zone>
// (networking.md §3): the name the desktop profile exposes and the
// component reconciler routes.
func consoleHost(zoneDomain string) string {
	return consoleSubdomain + "." + zoneDomain
}

func kernelHTTPRouteSpecs(
	kernelDomain string,
	tenantEffectiveDomains []string,
	tenantOIDCSubdomains map[string][]string,
	tenantNames []string,
	llmEnabled bool,
	cluster string,
	kernelZoneReady bool,
	desktop bool,
) []kernelHTTPRouteSpec {
	idHost := fmt.Sprintf("id.%s", kernelDomain)

	kcService := suzeKeycloakHTTPServiceName()
	kcPort := int32(8080)

	idFilters := keycloakGatewayResponseFilters(kernelDomain, tenantEffectiveDomains, tenantOIDCSubdomains, tenantNames)
	specs := []kernelHTTPRouteSpec{
		// The identity provider is a kernel-owned perimeter surface: no
		// session, because it is the issuer, and a path allowlist, because it
		// is public. Realm endpoints and the theme assets they load are the
		// whole of it; the master realm is refused by name, and the
		// administration console is a route of its own on this same hostname
		// behind the kernel session (networking.md §3).
		{
			name:        kernelRouteKeycloakIDP,
			host:        idHost,
			gateway:     PerimeterGatewayName,
			sectionName: perimeterIDListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, "/auth/realms/", idFilters...),
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, "/auth/resources/", idFilters...),
			},
			policy: keycloakProxyBackendTrafficPolicySpec(),
		},
		// What id.<kernel> refuses, as a route: these prefixes are more
		// specific than the allow above, so Gateway API ranks them first, and
		// the policy on them denies every caller. A refusal that is an object
		// can be read, listed and tested; an absence cannot.
		{
			name:        kernelRouteKeycloakRefused,
			host:        idHost,
			gateway:     PerimeterGatewayName,
			sectionName: perimeterIDListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, "/auth/realms/master/"),
			},
			securityPolicy: map[string]interface{}{
				"authorization": map[string]interface{}{"defaultAction": "Deny"},
			},
		},
	}
	clusterObject := "cluster:" + cluster
	if kernelZoneReady {
		// Keycloak's administration, on the SAME hostname that issues the
		// tokens, behind the kernel session.
		//
		// It had a hostname of its own, id-admin.<kernel>, which is the shape
		// every other kernel UI has. Keycloak cannot serve it that way: with
		// KC_HOSTNAME and KC_HOSTNAME_ADMIN different, the console takes a
		// token stamped with the issuer's hostname and calls the Admin REST
		// API on the admin hostname, which refuses it, and the console never
		// finishes loading. Upstream closed that as not planned
		// (keycloak/keycloak#42264), so it is a constraint and not a bug to
		// wait out. One hostname carrying two classes of route, told apart by
		// path, is the only shape that works.
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteKeycloakAdmin,
			host:        idHost,
			gateway:     PerimeterGatewayName,
			sectionName: perimeterIDListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, "/auth/admin/",
					kernelConsoleFrameFilters(kernelDomain)...),
				// The zone's code flow lands on /oauth2/callback: the OIDC
				// filter answers it, but only on a path the route carries.
				kernelBackendRulePrefixNS(kcService, identityNamespace, kcPort, edgeOAuth2Prefix),
			},
			policy: keycloakProxyBackendTrafficPolicySpec(),
			// The bearer on this route is the console's own. Keep it; do not
			// replace it.
			//
			// Keycloak's administration console runs its own code flow inside
			// the page and calls the Admin REST API with the token that flow
			// produced. Stripping Authorization -- what every other kernel
			// route wants, so a backend gets identity headers instead of a
			// token it has no use for -- answered 401 and left the console on
			// its spinner. Forwarding the EDGE's token instead answered "Token
			// issued for an application that is not the admin console", which
			// is true: it was minted for the zone's client. The console needs
			// neither, only to be left alone.
			authz: &routeAuthz{relation: "can_configure", object: clusterObject, keepClientToken: true},
		})
	}
	// The desktop is the tenant's own component, routed where it runs
	// (tenant-<t>, on console.<zone>); the kernel routes two names to it.
	// www.<kernel> and the apex are the names people type, and both send the
	// browser to the platform console with path and query kept: a session
	// travels in cookies on the kernel domain, so nothing is lost on the way.
	// Only once the kernel zone exists, because the console does not before.
	if kernelZoneReady && desktop {
		console := consoleHost(kernelDomain)
		specs = append(specs,
			kernelHTTPRouteSpec{
				name:        kernelRouteWWWRedirect,
				host:        "www." + kernelDomain,
				sectionName: wildcardListenerName,
				rules:       []gatewayv1.HTTPRouteRule{consoleRedirectRule(console)},
			},
			kernelHTTPRouteSpec{
				name:        kernelRouteKernelApex,
				host:        kernelDomain,
				sectionName: wildcardListenerName,
				rules:       []gatewayv1.HTTPRouteRule{consoleRedirectRule(console)},
			},
		)
	}
	// A tenant's apex likewise sends the browser to the tenant's own console.
	// The apex is published with the tenant either way (it is the tenant's
	// name), so the redirect is what makes it answer.
	if desktop {
		for i, domain := range tenantEffectiveDomains {
			if i >= len(tenantNames) {
				break
			}
			specs = append(specs, kernelHTTPRouteSpec{
				name:        fmt.Sprintf("tenant-%s-apex", tenantNames[i]),
				host:        domain,
				sectionName: wildcardListenerName,
				rules:       []gatewayv1.HTTPRouteRule{consoleRedirectRule(consoleHost(domain))},
			})
		}
	}
	specs = append(specs,
		kernelHTTPRouteSpec{
			// Plaintext :80 -> https, bound to the http-redirect listener only.
			name:        kernelRouteHTTPRedirect,
			gateway:     PerimeterGatewayName,
			sectionName: httpRedirectListenerName,
			rules:       []gatewayv1.HTTPRouteRule{kernelHTTPSRedirectRule()},
		},
	)
	// The kernel UIs: "hidden" means behind a session with a platform role,
	// not an internal hostname, and each tool's own login is the second
	// factor (networking.md §3). No zone, no route: never an open one.
	if kernelZoneReady {
		specs = append(specs, kernelHTTPRouteSpec{
			name:        kernelRouteArgoCD,
			host:        fmt.Sprintf("argocd.%s", kernelDomain),
			sectionName: wildcardListenerName,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRuleCrossNamespace(argocdServerServiceName, argocdNamespace, 80,
					kernelConsoleFrameFilters(kernelDomain)...),
			},
			authz: &routeAuthz{relation: "can_configure", object: clusterObject},
		})
		// The cluster view, read-only: an auditor's right. A route the
		// operator owns is the one that gets a tunnel hostname and a DNS
		// record; only where the layout has an observability namespace, since
		// a route to a Service that does not exist would still claim the host.
		if observabilityNamespace != "" {
			specs = append(specs, kernelHTTPRouteSpec{
				name:        kernelRouteHeadlamp,
				host:        fmt.Sprintf("headlamp.%s", kernelDomain),
				sectionName: wildcardListenerName,
				rules: []gatewayv1.HTTPRouteRule{
					kernelBackendRuleCrossNamespace(headlampServiceName, observabilityNamespace, 80,
						kernelConsoleFrameFilters(kernelDomain)...),
				},
				authz: &routeAuthz{relation: "can_audit", object: clusterObject},
			})
		}
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

// kernelConsoleFrameFilters lets the desktop open a kernel console in a window.
//
// Each console defends itself against being framed, which is right against a
// stranger and wrong here: the desktop, the console and the realm are all on
// the kernel domain, and the tile is how a platform administrator is meant to
// reach it. Argo CD is the strict one -- x-frame-options: sameorigin, which it
// cannot be told to drop, because an empty setting falls back to the default.
//
// So the Gateway decides, for every kernel host the same way: the header that
// cannot express an exception is removed, and the one that can names the
// kernel domain and nothing else.
func kernelConsoleFrameFilters(kernelDomain string) []gatewayv1.HTTPRouteFilter {
	if kernelDomain == "" {
		return nil
	}
	return []gatewayv1.HTTPRouteFilter{{
		Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Remove: []string{"X-Frame-Options"},
			Set: []gatewayv1.HTTPHeader{{
				Name:  "Content-Security-Policy",
				Value: fmt.Sprintf("frame-ancestors 'self' https://*.%s", kernelDomain),
			}},
		},
	}}
}

func kernelBackendRuleCrossNamespace(serviceName, namespace string, port int32, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	p := gatewayv1.PortNumber(port)
	ns := gatewayv1.Namespace(namespace)
	return gatewayv1.HTTPRouteRule{
		Filters: filters,
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

// consoleRedirectRule sends every request on a host to the console, keeping
// path and query: a redirect that replaced them dropped whatever travelled on
// the request, which is why the portal used to be served on these names
// rather than redirected. Nothing travels on them now -- the session is in
// cookies on the zone's domain -- so an alias by redirect loses nothing.
func consoleRedirectRule(console string) gatewayv1.HTTPRouteRule {
	scheme := "https"
	status := 302
	port := gatewayv1.PortNumber(443)
	host := gatewayv1.PreciseHostname(console)
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
		Filters: []gatewayv1.HTTPRouteFilter{
			{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Scheme:     &scheme,
					Hostname:   &host,
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
	gateway := spec.gateway
	if gateway == "" {
		gateway = AuthenticatedGatewayName
	}
	parentRef := gatewayParentRef(gateway)
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
			"name":        AuthenticatedGatewayName,
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
