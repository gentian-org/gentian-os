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
	"fmt"
	"net/url"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	gatewayComponentLabel    = "gentianos.io/gateway-component"
	gatewayComponentApp      = "app-route"
	gatewayComponentApex     = "apex-redirect"
	gatewayComponentKernel   = "kernel-route"
)

type ingressIntent struct {
	appProfile string
	profile    *gentianov1alpha1.AppProfile
	ingress    *gentianov1alpha1.IngressSpec
}

func appHTTPRouteName(tenantName, appProfile string) string {
	return fmt.Sprintf("httproute-%s-%s", tenantName, appProfile)
}

func appBackendTrafficPolicyName(tenantName, appProfile string) string {
	return fmt.Sprintf("btp-%s-%s", tenantName, appProfile)
}

func tenantEscapedSlashesClientTrafficPolicyName(tenantName string) string {
	return fmt.Sprintf("ctp-%s-escaped-slashes", tenantName)
}

func tenantApexRedirectRouteName(tenantName string) string {
	return tenantPortalRedirectName(tenantName)
}

func gatewayParentRef(gatewayName string) gatewayv1.ParentReference {
	g := gatewayv1.Group("gateway.networking.k8s.io")
	k := gatewayv1.Kind("Gateway")
	return gatewayv1.ParentReference{
		Group: &g,
		Kind:  &k,
		Name:  gatewayv1.ObjectName(gatewayName),
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

// appHTTPRoutesForIntents builds the desired per-app HTTPRoutes for a tenant.
func appHTTPRoutesForIntents(
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	intents []ingressIntent,
	effectiveDomain, kernelDomain string,
) []*gatewayv1.HTTPRoute {
	routes := make([]*gatewayv1.HTTPRoute, 0, len(intents))
	for _, intent := range intents {
		routes = append(routes, buildAppHTTPRoute(tenant, nsName, intent, effectiveDomain, kernelDomain))
	}
	return routes
}

func buildAppHTTPRoute(
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	intent ingressIntent,
	effectiveDomain, kernelDomain string,
) *gatewayv1.HTTPRoute {
	host := ingressHost(intent.appProfile, intent.ingress, effectiveDomain)
	ingress := intent.ingress
	svcName := ingress.ServiceName
	if svcName == "" {
		svcName = intent.appProfile
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

	if intent.profile != nil && gentianov1alpha1.ProfileIsAPI(intent.profile) && intent.profile.Spec.APIIntegration != nil {
		api := intent.profile.Spec.APIIntegration
		switch api.Runtime {
		case gentianov1alpha1.APIIntegrationRuntimeRedirect:
			u, err := url.Parse(api.BaseURL)
			if err == nil {
				scheme := u.Scheme
				if scheme == "" {
					scheme = "https"
				}
				hostname := gatewayv1.PreciseHostname(u.Hostname())

				var portVal gatewayv1.PortNumber
				if u.Port() != "" {
					p, _ := strconv.Atoi(u.Port())
					portVal = gatewayv1.PortNumber(p)
				} else if scheme == "https" {
					portVal = 443
				} else {
					portVal = 80
				}

				pathType := gatewayv1.FullPathHTTPPathModifier
				targetPath := u.Path
				if targetPath == "" {
					targetPath = "/"
				}
				if api.TenantBinding == gentianov1alpha1.APIIntegrationTenantBindingDomain {
					sep := "?"
					if strings.Contains(targetPath, "?") || u.RawQuery != "" {
						sep = "&"
					}
					rawQuery := u.RawQuery
					if rawQuery != "" {
						targetPath += sep + rawQuery + "&tenantDomain=" + effectiveDomain
					} else {
						targetPath += sep + "tenantDomain=" + effectiveDomain
					}
				} else if u.RawQuery != "" {
					targetPath += "?" + u.RawQuery
				}

				statusCode := 302
				rule = gatewayv1.HTTPRouteRule{
					Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
					Filters: []gatewayv1.HTTPRouteFilter{{
						Type: gatewayv1.HTTPRouteFilterRequestRedirect,
						RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
							Scheme:     &scheme,
							Hostname:   &hostname,
							Port:       &portVal,
							StatusCode: &statusCode,
							Path: &gatewayv1.HTTPPathModifier{
								Type:            pathType,
								ReplaceFullPath: &targetPath,
							},
						},
					}},
				}
			}
		case gentianov1alpha1.APIIntegrationRuntimePortalProxy:
			portVal := gatewayv1.PortNumber(8000)
			rule = gatewayv1.HTTPRouteRule{
				Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch("/")},
				BackendRefs: []gatewayv1.HTTPBackendRef{
					{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: gatewayv1.ObjectName("gentian-portal-gentian-portal-api"),
								Port: &portVal,
							},
						},
					},
				},
			}
		}
	}
	mainIngressSubDomain := ""
	if intent.profile != nil && intent.profile.Spec.Ingress != nil {
		mainIngressSubDomain = intent.profile.Spec.Ingress.SubDomain
	}
	if filters := gatewayEmbeddingResponseFilters(kernelDomain, effectiveDomain, ingress.SubDomain, mainIngressSubDomain, ingress); len(filters) > 0 {
		rule.Filters = filters
	}

	rules := []gatewayv1.HTTPRouteRule{rule}
	if apiRules := appAPIBackendRules(intent.profile, svcPort, kernelDomain, effectiveDomain, intent.ingress); len(apiRules) > 0 {
		rules = append(apiRules, rules...)
	}
	if rootRedirect := appRootRedirectRule(intent.profile, host); rootRedirect != nil {
		rules = append([]gatewayv1.HTTPRouteRule{*rootRedirect}, rules...)
	}

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appHTTPRouteName(tenant.Name, intent.appProfile),
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:           tenant.Name,
				appLabel:              intent.appProfile,
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

func appRootRedirectRule(profile *gentianov1alpha1.AppProfile, host string) *gatewayv1.HTTPRouteRule {
	target := gentianov1alpha1.ProfileGatewayRootRedirect(profile)
	if target == "" {
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

func appAPIBackendRules(
	profile *gentianov1alpha1.AppProfile,
	defaultPort int32,
	kernelDomain, effectiveDomain string,
	ingress *gentianov1alpha1.IngressSpec,
) []gatewayv1.HTTPRouteRule {
	backends, err := gentianov1alpha1.ProfileGatewayAPIBackends(profile)
	if err != nil || len(backends) == 0 {
		return nil
	}
	mainIngressSubDomain := ""
	if profile != nil && profile.Spec.Ingress != nil {
		mainIngressSubDomain = profile.Spec.Ingress.SubDomain
	}
	var filters []gatewayv1.HTTPRouteFilter
	if ingress != nil {
		filters = gatewayEmbeddingResponseFilters(kernelDomain, effectiveDomain, ingress.SubDomain, mainIngressSubDomain, ingress)
	}
	var rules []gatewayv1.HTTPRouteRule
	for _, backend := range backends {
		if backend.PathPrefix == "" || backend.ServiceName == "" {
			continue
		}
		port := defaultPort
		if backend.Port > 0 {
			port = backend.Port
		}
		p := gatewayv1.PortNumber(port)
		prefix := backend.PathPrefix
		rule := gatewayv1.HTTPRouteRule{
			Matches: []gatewayv1.HTTPRouteMatch{pathPrefixMatch(prefix)},
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(backend.ServiceName),
						Port: &p,
					},
				},
			}},
		}
		if len(filters) > 0 {
			rule.Filters = filters
		}
		rules = append(rules, rule)
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
			Labels:    portalRedirectLabels(tenant.Name, tenantApexRedirectRouteName(tenant.Name)),
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
								Scheme:   &scheme,
								Hostname: &portalHost,
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

func gatewayEmbeddingResponseFilters(
	kernelDomain, effectiveDomain, ingressSubDomain, mainIngressSubDomain string,
	ingress *gentianov1alpha1.IngressSpec,
) []gatewayv1.HTTPRouteFilter {
	policy := computeGatewayFrameAncestorsPolicy(kernelDomain, effectiveDomain, ingressSubDomain)
	if custom, ok, err := ingressFrameAncestorsPolicy(kernelDomain, effectiveDomain, mainIngressSubDomain, ingress); err == nil && ok {
		policy = custom
	}
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

func computeGatewayFrameAncestorsPolicy(kernelDomain, effectiveDomain, _ string) gatewayFrameAncestorsPolicy {
	var origins []string
	if kernelDomain != "" {
		origins = append(origins, fmt.Sprintf("https://portal.%s", kernelDomain))
	}
	if effectiveDomain != "" && effectiveDomain != kernelDomain {
		origins = append(origins, fmt.Sprintf("https://%s", effectiveDomain))
		origins = append(origins, fmt.Sprintf("https://*.%s", effectiveDomain))
	}
	if len(origins) > 0 {
		return gatewayFrameAncestorsPolicy{
			Mode:    gatewayFrameAncestorsReplace,
			Origins: strings.Join(origins, " "),
		}
	}
	return gatewayFrameAncestorsPolicy{}
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
