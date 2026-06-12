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
	kernelRouteKeycloakIDP         = "kernel-idp"
	kernelRoutePortal              = "kernel-portal"
	kernelRoutePortalRewrites      = "kernel-portal-rewrite-assets"
	kernelRoutePortalRewritesApp   = "kernel-portal-rewrite-app"
	kernelRoutePortalLogin         = "kernel-portal-login"
	kernelRoutePortalBaseRouter    = "kernel-portal-base-router"
	kernelRoutePortalServer        = "kernel-portal-server"
	kernelRoutePortalUMC           = "kernel-portal-umc"
	kernelRoutePortalUDM           = "kernel-portal-udm"
	kernelRouteKernelApex          = "kernel-apex-redirect"
	kernelRouteCryptpad            = "kernel-cryptpad"
	kernelRouteCryptpadSandbox     = "kernel-cryptpad-sandbox"
	kernelRouteNextcloud           = "kernel-nextcloud"
	kernelRouteCollabora           = "kernel-collabora"
	kernelRouteIntercom            = "kernel-intercom"

	keycloakProxyServicePort = int32(8181)
	baseRouterServicePort    = int32(8080)
	udmRestAPIServicePort    = int32(9979)
)

type kernelHTTPRouteSpec struct {
	name   string
	host   string
	rules  []gatewayv1.HTTPRouteRule
	policy map[string]interface{}
}

func kernelStage() string {
	return envOrDefault("GENTIAN_STAGE", envOrDefault("ENV", "dev"))
}

func nubusReleaseName() string {
	return fmt.Sprintf("nubus-%s", kernelStage())
}

func keycloakProxyServiceName() string {
	if v := envOrDefault("KEYCLOAK_PROXY_SERVICE", ""); v != "" {
		return v
	}
	return fmt.Sprintf("%s-keycloak-extensions-proxy", nubusReleaseName())
}

func portalFrontendServiceName() string {
	if v := envOrDefault("PORTAL_FRONTEND_SERVICE", ""); v != "" {
		return v
	}
	return fmt.Sprintf("%s-portal-frontend", nubusReleaseName())
}

func gentianLoginServiceName() string {
	return fmt.Sprintf("gentian-portal-gentian-login-%s-gentian-login", kernelStage())
}

func baseRouterServiceName() string {
	return fmt.Sprintf("gentian-portal-base-router-%s-base-router", kernelStage())
}

func portalServerServiceName() string {
	return fmt.Sprintf("%s-portal-server", nubusReleaseName())
}

func umcGatewayServiceName() string {
	return fmt.Sprintf("%s-umc-gateway", nubusReleaseName())
}

func udmRestAPIServiceName() string {
	return fmt.Sprintf("%s-udm-rest-api", nubusReleaseName())
}

func nextcloudServiceName() string {
	return fmt.Sprintf("nextcloud-%s-aio", kernelStage())
}

func intercomServiceName() string {
	return fmt.Sprintf("intercom-service-%s", kernelStage())
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
	filesHost := fmt.Sprintf("files.%s", kernelDomain)
	officeHost := fmt.Sprintf("office.%s", kernelDomain)
	icsHost := fmt.Sprintf("ics.%s", kernelDomain)

	return []kernelHTTPRouteSpec{
		{
			name: kernelRouteKeycloakIDP,
			host: idHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRule(keycloakProxyServiceName(), keycloakProxyServicePort, keycloakGatewayResponseFilters(kernelDomain, tenantEffectiveDomains, tenantOIDCSubdomains, tenantNames)),
			},
			policy: keycloakProxyBackendTrafficPolicySpec(),
		},
		{
			name: kernelRoutePortalLogin,
			host: portalHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefix(gentianLoginServiceName(), 80, "/login"),
			},
		},
		{
			name: kernelRoutePortalBaseRouter,
			host: portalHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefix(baseRouterServiceName(), baseRouterServicePort, "/u/base-router"),
			},
		},
		{
			name: kernelRoutePortalServer,
			host: portalHost,
			rules: kernelPortalServerDataRules(portalServerServiceName(), 80),
		},
		{
			name: kernelRoutePortalUMC,
			host: portalHost,
			rules: kernelUMCGatewayRules(umcGatewayServiceName(), 80),
		},
		{
			name: kernelRoutePortalUDM,
			host: portalHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRulePrefix(udmRestAPIServiceName(), udmRestAPIServicePort, "/univention/udm"),
			},
		},
		{
			name: kernelRoutePortalRewrites,
			host: portalHost,
			rules: kernelPortalFrontendAssetRewriteRules(portalFrontendServiceName(), 80),
		},
		{
			name: kernelRoutePortalRewritesApp,
			host: portalHost,
			rules: kernelPortalFrontendAppRewriteRules(portalFrontendServiceName(), 80),
		},
		{
			name: kernelRoutePortal,
			host: portalHost,
			rules: kernelPortalFrontendRules(portalFrontendServiceName(), 80),
		},
		{
			name: kernelRouteKernelApex,
			host: kernelDomain,
			rules: []gatewayv1.HTTPRouteRule{
				kernelApexRedirectRule(kernelDomain),
			},
		},
		{
			name: kernelRouteCryptpad,
			host: padHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRule("cryptpad", 80, kernelCryptpadMainResponseFilters(kernelDomain)),
			},
			policy: cryptpadBackendTrafficPolicySpec(),
		},
		{
			name: kernelRouteCryptpadSandbox,
			host: padSandboxHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRule("cryptpad", 80, kernelCryptpadSandboxResponseFilters(kernelDomain)),
			},
			policy: cryptpadBackendTrafficPolicySpec(),
		},
		{
			name: kernelRouteNextcloud,
			host: filesHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRule(nextcloudServiceName(), 80, kernelNextcloudResponseFilters(kernelDomain)),
			},
		},
		{
			name: kernelRouteCollabora,
			host: officeHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRule("collabora", 9980, nil),
			},
			policy: collaboraBackendTrafficPolicySpec(),
		},
		{
			name: kernelRouteIntercom,
			host: icsHost,
			rules: []gatewayv1.HTTPRouteRule{
				kernelBackendRule(intercomServiceName(), 8008, nil),
			},
		},
	}
}

func kernelPortalServerDataRules(serviceName string, port int32) []gatewayv1.HTTPRouteRule {
	// Mirror portal-server ingress paths (portal.json, navigation.json, api/me).
	exactPaths := []string{
		"/univention/portal/portal.json",
		"/univention/portal/navigation.json",
		"/univention/selfservice/portal.json",
		"/univention/selfservice/navigation.json",
	}
	rules := make([]gatewayv1.HTTPRouteRule, 0, len(exactPaths)+2)
	for _, path := range exactPaths {
		rules = append(rules, kernelBackendRuleExact(serviceName, port, path))
	}
	rules = append(rules,
		kernelBackendRulePrefix(serviceName, port, "/univention/portal/api/v1/me"),
		kernelBackendRulePrefix(serviceName, port, "/univention/portal-server"),
	)
	return rules
}

func kernelUMCGatewayRules(serviceName string, port int32) []gatewayv1.HTTPRouteRule {
	rules := []gatewayv1.HTTPRouteRule{
		kernelBackendRuleExact(serviceName, port, "/univention/meta.json"),
		kernelBackendRuleExact(serviceName, port, "/univention/languages.json"),
	}
	// Match Apache ProxyPassMatch in the umc-gateway image:
	// ^/univention/((auth|saml|oidc|get|set|command|upload|logout|logout-sse)/?.*)$
	prefixes := []string{
		"auth", "saml", "oidc", "get", "set", "command", "upload", "logout", "logout-sse", "umc",
	}
	for _, segment := range prefixes {
		rules = append(rules, kernelBackendRulePrefix(serviceName, port, fmt.Sprintf("/univention/%s", segment)))
	}
	return rules
}

func kernelPortalFrontendRules(serviceName string, port int32) []gatewayv1.HTTPRouteRule {
	return []gatewayv1.HTTPRouteRule{
		kernelRedirectRule("/univention/portal", "/univention/portal/", 301),
		kernelRedirectRule("/univention/selfservice", "/univention/portal/", 301),
		kernelRedirectRulePrefix("/univention/login", "/login/", 301),
		kernelBackendRulePrefix(serviceName, port, "/univention/portal"),
		kernelBackendRulePrefix(serviceName, port, "/univention/selfservice"),
		kernelBackendRulePrefix(serviceName, port, "/"),
	}
}

// kernelPortalFrontendAssetRewriteRules mirrors nginx rewrite-target=/$2$3 for
// static asset paths. Split into a separate HTTPRoute to stay within the 16-rule limit.
func kernelPortalFrontendAssetRewriteRules(serviceName string, port int32) []gatewayv1.HTTPRouteRule {
	assetSegments := []string{"css", "fonts", "i18n", "media", "js", "oidc", "custom"}
	appPrefixes := []string{"portal", "selfservice"}
	var rules []gatewayv1.HTTPRouteRule
	for _, app := range appPrefixes {
		for _, segment := range assetSegments {
			rules = append(rules, kernelURLRewriteBackendRule(
				serviceName, port,
				fmt.Sprintf("/univention/%s/%s", app, segment),
				fmt.Sprintf("/%s", segment),
			))
		}
	}
	rules = append(rules, kernelURLRewriteBackendRule(
		serviceName, port,
		"/univention/portal/icons",
		"/icons",
	))
	return rules
}

func kernelPortalFrontendAppRewriteRules(serviceName string, port int32) []gatewayv1.HTTPRouteRule {
	appPrefixes := []string{"portal", "selfservice"}
	var rules []gatewayv1.HTTPRouteRule
	for _, app := range appPrefixes {
		rules = append(rules, kernelExactURLRewriteBackendRule(
			serviceName, port,
			fmt.Sprintf("/univention/%s/index.html", app),
			"/index.html",
		))
		rules = append(rules, kernelExactURLRewriteBackendRule(
			serviceName, port,
			fmt.Sprintf("/univention/%s/sse-worker.js", app),
			"/sse-worker.js",
		))
		rules = append(rules, kernelURLRewriteBackendRule(
			serviceName, port,
			fmt.Sprintf("/univention/%s/", app),
			"/",
		))
	}
	return rules
}

// kernelPortalFrontendRewriteRules returns all rewrite rules (used in tests).
func kernelPortalFrontendRewriteRules(serviceName string, port int32) []gatewayv1.HTTPRouteRule {
	rules := kernelPortalFrontendAssetRewriteRules(serviceName, port)
	return append(rules, kernelPortalFrontendAppRewriteRules(serviceName, port)...)
}

func kernelURLRewriteBackendRule(serviceName string, port int32, matchPrefix, replacePrefix string) gatewayv1.HTTPRouteRule {
	replacement := replacePrefix
	rule := kernelBackendRulePrefix(serviceName, port, matchPrefix)
	rule.Filters = []gatewayv1.HTTPRouteFilter{
		{
			Type: gatewayv1.HTTPRouteFilterURLRewrite,
			URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
				Path: &gatewayv1.HTTPPathModifier{
					Type:               gatewayv1.PrefixMatchHTTPPathModifier,
					ReplacePrefixMatch: &replacement,
				},
			},
		},
	}
	return rule
}

func kernelExactURLRewriteBackendRule(serviceName string, port int32, matchPath, replacePath string) gatewayv1.HTTPRouteRule {
	replacement := replacePath
	pathType := gatewayv1.FullPathHTTPPathModifier
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathExactMatch(matchPath)},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(serviceName),
						Port: ptrPortNumber(port),
					},
				},
			},
		},
		Filters: []gatewayv1.HTTPRouteFilter{
			{
				Type: gatewayv1.HTTPRouteFilterURLRewrite,
				URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
					Path: &gatewayv1.HTTPPathModifier{
						Type:            pathType,
						ReplaceFullPath: &replacement,
					},
				},
			},
		},
	}
	return rule
}

func ptrPortNumber(port int32) *gatewayv1.PortNumber {
	p := gatewayv1.PortNumber(port)
	return &p
}

func kernelBackendRule(serviceName string, port int32, filters []gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return kernelBackendRulePrefix(serviceName, port, "/", filters...)
}

func kernelBackendRulePrefix(serviceName string, port int32, prefix string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	p := gatewayv1.PortNumber(port)
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch(prefix)},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(serviceName),
						Port: &p,
					},
				},
			},
		},
	}
	if len(filters) > 0 {
		rule.Filters = filters
	}
	return rule
}

func kernelBackendRuleExact(serviceName string, port int32, path string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	p := gatewayv1.PortNumber(port)
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathExactMatch(path)},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(serviceName),
						Port: &p,
					},
				},
			},
		},
	}
	if len(filters) > 0 {
		rule.Filters = filters
	}
	return rule
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

func kernelRedirectRulePrefix(pathPrefix, target string, status int) gatewayv1.HTTPRouteRule {
	pathType := gatewayv1.FullPathHTTPPathModifier
	targetPath := target
	return gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch(pathPrefix)},
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

func kernelNextcloudResponseFilters(kernelDomain string) []gatewayv1.HTTPRouteFilter {
	portalHost := kernelPortalHost(kernelDomain)
	modifier := gatewayv1.HTTPHeaderFilter{
		Remove: []string{"X-Frame-Options", "Content-Security-Policy"},
		Set: []gatewayv1.HTTPHeader{
			{Name: "Content-Security-Policy", Value: fmt.Sprintf("frame-ancestors 'self' https://%s", portalHost)},
		},
	}
	return []gatewayv1.HTTPRouteFilter{
		{
			Type:                   gatewayv1.HTTPRouteFilterResponseHeaderModifier,
			ResponseHeaderModifier: &modifier,
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

func collaboraBackendTrafficPolicySpec() map[string]interface{} {
	return map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
			},
		},
		"timeout": map[string]interface{}{
			"http": map[string]interface{}{
				"requestTimeout":  "600s",
				"responseTimeout": "600s",
			},
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
