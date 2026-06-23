// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	gatewayComponentLabel     = "gentianos.io/gateway-component"
	gatewayComponentApp       = "app-route"
	gatewayComponentApex      = "apex-redirect"
	gatewayComponentKernel    = "kernel-route"
	umcPortalRedirectGateway  = "umc-frontend"
)

type ingressIntent struct {
	appProfile string
	ingress    *gentianov1alpha1.IngressSpec
}

func appHTTPRouteName(tenantName, appProfile string) string {
	return fmt.Sprintf("httproute-%s-%s", tenantName, appProfile)
}

func appBackendTrafficPolicyName(tenantName, appProfile string) string {
	return fmt.Sprintf("btp-%s-%s", tenantName, appProfile)
}

func tenantApexRedirectRouteName(tenantName string) string {
	return tenantPortalRedirectName(tenantName)
}

func gatewayParentRef(gatewayName string) gatewayv1.ParentReference {
	g := gatewayv1.Group("gateway.networking.k8s.io")
	k := gatewayv1.Kind("Gateway")
	return gatewayv1.ParentReference{
		Group:     &g,
		Kind:      &k,
		Name:      gatewayv1.ObjectName(gatewayName),
	}
}

func kernelGatewayParentRef() gatewayv1.ParentReference {
	ref := gatewayParentRef(KernelPublicGatewayName)
	ns := gatewayv1.Namespace(servicesNamespace)
	ref.Namespace = &ns
	return ref
}

func tenantGatewayParentRefs(tenantName string) []gatewayv1.ParentReference {
	return []gatewayv1.ParentReference{
		gatewayParentRef(tenantGatewayName(tenantName)),
		kernelGatewayParentRef(),
	}
}

func pathPrefixMatch(prefix string) gatewayv1.HTTPRouteMatch {
	t := gatewayv1.PathMatchPathPrefix
	return gatewayv1.HTTPRouteMatch{
		Path: &gatewayv1.HTTPPathMatch{
			Type:  &t,
			Value: &prefix,
		},
	}
}

func buildAppHTTPRoute(
	tenant *gentianov1alpha1.Tenant,
	nsName, appProfile string,
	ingress *gentianov1alpha1.IngressSpec,
	host, effectiveDomain, kernelDomain string,
) *gatewayv1.HTTPRoute {
	svcName := ingress.ServiceName
	if svcName == "" {
		svcName = appProfile
	}
	svcPort := ingress.ServicePort
	if svcPort == 0 {
		svcPort = defaultServicePort
	}
	port := gatewayv1.PortNumber(svcPort)

	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(svcName),
						Port: &port,
					},
				},
			},
		},
	}
	if filters := gatewayEmbeddingResponseFilters(kernelDomain, effectiveDomain, ingress.SubDomain); len(filters) > 0 {
		rule.Filters = filters
	}

	rules := []gatewayv1.HTTPRouteRule{rule}
	if apiRules := appAPIBackendRules(appProfile, svcPort); len(apiRules) > 0 {
		rules = append(apiRules, rules...)
	}
	if rootRedirect := appRootRedirectRule(appProfile, host); rootRedirect != nil {
		rules = append([]gatewayv1.HTTPRouteRule{*rootRedirect}, rules...)
	}

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appHTTPRouteName(tenant.Name, appProfile),
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:           tenant.Name,
				appLabel:              appProfile,
				managedByLabel:        managedByValue,
				gatewayComponentLabel: gatewayComponentApp,
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: tenantGatewayParentRefs(tenant.Name),
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(host)},
			Rules:     rules,
		},
	}
}

func appRootRedirectRule(appProfile, host string) *gatewayv1.HTTPRouteRule {
	target, ok := appRootRedirectTargets[appProfile]
	if !ok {
		return nil
	}
	scheme := "https"
	status := 302
	pathType := gatewayv1.FullPathHTTPPathModifier
	port := gatewayv1.PortNumber(443)
	exactPath := gatewayv1.PathMatchExact
	rootPath := "/"
	hostname := gatewayv1.PreciseHostname(host)
	return &gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path: &gatewayv1.HTTPPathMatch{
				Type:  &exactPath,
				Value: &rootPath,
			},
		}},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Scheme:   &scheme,
				Hostname: &hostname,
				Path: &gatewayv1.HTTPPathModifier{
					Type:            pathType,
					ReplaceFullPath: &target,
				},
				Port:       &port,
				StatusCode: &status,
			},
		}},
	}
}

var appRootRedirectTargets = map[string]string{
	"ox-appsuite": "/appsuite/",
}

type appAPIBackend struct {
	pathPrefix  string
	serviceName string
}

var appAPIBackends = map[string][]appAPIBackend{
	"ox-appsuite": {
		{pathPrefix: "/appsuite/api", serviceName: "appsuite-api"},
	},
}

func appAPIBackendRules(appProfile string, svcPort int32) []gatewayv1.HTTPRouteRule {
	backends, ok := appAPIBackends[appProfile]
	if !ok {
		return nil
	}
	port := gatewayv1.PortNumber(svcPort)
	var rules []gatewayv1.HTTPRouteRule
	for _, backend := range backends {
		prefix := backend.pathPrefix
		rules = append(rules, gatewayv1.HTTPRouteRule{
			Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch(prefix)},
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(backend.serviceName),
						Port: &port,
					},
				},
			}},
		})
	}
	return rules
}

func buildTenantApexRedirectHTTPRoute(tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain, kernelDomain string) *gatewayv1.HTTPRoute {
	scheme := "https"
	status := 302
	port := gatewayv1.PortNumber(443)
	pathType := gatewayv1.FullPathHTTPPathModifier
	loginPath := "/login/"
	portalHost := gatewayv1.PreciseHostname(kernelPortalHost(kernelDomain))
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantApexRedirectRouteName(tenant.Name),
			Namespace: nsName,
			Labels:    umcPortalRedirectLabels(tenant.Name, tenantApexRedirectRouteName(tenant.Name)),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: tenantGatewayParentRefs(tenant.Name),
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(effectiveDomain)},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
					Filters: []gatewayv1.HTTPRouteFilter{
						{
							Type: gatewayv1.HTTPRouteFilterRequestRedirect,
							RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
								Scheme:     &scheme,
								Hostname:   &portalHost,
								Path: &gatewayv1.HTTPPathModifier{
									Type:            pathType,
									ReplaceFullPath: &loginPath,
								},
								Port:       &port,
								StatusCode: &status,
							},
						},
					},
				},
			},
		},
	}
}

func gatewayEmbeddingResponseFilters(kernelDomain, effectiveDomain, ingressSubDomain string) []gatewayv1.HTTPRouteFilter {
	policy := computeGatewayFrameAncestorsPolicy(kernelDomain, effectiveDomain, ingressSubDomain)
	if policy.Origins == "" {
		return nil
	}
	modifier := gatewayv1.HTTPHeaderFilter{
		Remove: []string{"X-Frame-Options"},
	}
	switch policy.Mode {
	case gatewayFrameAncestorsAppend:
		modifier.Add = []gatewayv1.HTTPHeader{
			{Name: "Content-Security-Policy", Value: fmt.Sprintf("frame-ancestors 'self' %s", policy.Origins)},
		}
	default:
		modifier.Set = []gatewayv1.HTTPHeader{{
			Name:  "Content-Security-Policy",
			Value: fmt.Sprintf("frame-ancestors 'self' %s", policy.Origins),
		}}
	}
	return []gatewayv1.HTTPRouteFilter{
		{
			Type:                   gatewayv1.HTTPRouteFilterResponseHeaderModifier,
			ResponseHeaderModifier: &modifier,
		},
	}
}

const (
	gatewayFrameAncestorsReplace = "replace"
	gatewayFrameAncestorsAppend  = "append"
)

type gatewayFrameAncestorsPolicy struct {
	Mode    string
	Origins string
}

func computeGatewayFrameAncestorsPolicy(kernelDomain, effectiveDomain, ingressSubDomain string) gatewayFrameAncestorsPolicy {
	switch {
	case ingressSubDomain == cryptpadSandboxSubDomain && effectiveDomain != "":
		return gatewayFrameAncestorsPolicy{
			Mode:    gatewayFrameAncestorsAppend,
			Origins: cryptpadSandboxFrameAncestorOrigins(kernelDomain, effectiveDomain),
		}
	case ingressSubDomain == cryptpadMainSubDomain && kernelDomain != "":
		return gatewayFrameAncestorsPolicy{
			Mode:    gatewayFrameAncestorsAppend,
			Origins: fmt.Sprintf("https://portal.%s", kernelDomain),
		}
	case kernelDomain != "":
		return gatewayFrameAncestorsPolicy{
			Mode:    gatewayFrameAncestorsReplace,
			Origins: fmt.Sprintf("https://portal.%s", kernelDomain),
		}
	default:
		return gatewayFrameAncestorsPolicy{}
	}
}

func keycloakGatewayResponseFilters(kernelDomain string, tenantEffectiveDomains []string, tenantOIDCSubdomains map[string][]string, tenantNames []string) []gatewayv1.HTTPRouteFilter {
	origins := keycloakOIDCAncestorOrigins(kernelDomain, tenantEffectiveDomains, tenantOIDCSubdomains, tenantNames)
	if origins == "" {
		return nil
	}
	modifier := gatewayv1.HTTPHeaderFilter{
		Remove: []string{"X-Frame-Options", "Content-Security-Policy"},
		Set: []gatewayv1.HTTPHeader{
			{Name: "Content-Security-Policy", Value: fmt.Sprintf("frame-ancestors 'self' %s", origins)},
		},
	}
	return []gatewayv1.HTTPRouteFilter{
		{
			Type:                   gatewayv1.HTTPRouteFilterResponseHeaderModifier,
			ResponseHeaderModifier: &modifier,
		},
	}
}

func kernelCryptpadMainResponseFilters(kernelDomain string) []gatewayv1.HTTPRouteFilter {
	return cryptpadGatewayAppendFrameAncestorsFilters(cryptpadKernelMainFrameAncestorOrigins(kernelDomain))
}

func kernelCryptpadSandboxResponseFilters(kernelDomain string) []gatewayv1.HTTPRouteFilter {
	return cryptpadGatewayAppendFrameAncestorsFilters(cryptpadSandboxFrameAncestorOrigins(kernelDomain, kernelDomain))
}

// cryptpadGatewayAppendFrameAncestorsFilters appends a frame-ancestors CSP header
// without removing CryptPad's upstream policy (script-src must stay strict).
func cryptpadGatewayAppendFrameAncestorsFilters(origins string) []gatewayv1.HTTPRouteFilter {
	if origins == "" {
		return nil
	}
	modifier := gatewayv1.HTTPHeaderFilter{
		Remove: []string{"X-Frame-Options"},
		Add: []gatewayv1.HTTPHeader{
			{Name: "Content-Security-Policy", Value: fmt.Sprintf("frame-ancestors 'self' %s", origins)},
		},
	}
	return []gatewayv1.HTTPRouteFilter{
		{
			Type:                   gatewayv1.HTTPRouteFilterResponseHeaderModifier,
			ResponseHeaderModifier: &modifier,
		},
	}
}
