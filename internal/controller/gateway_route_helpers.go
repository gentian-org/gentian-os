/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"fmt"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	gatewayComponentLabel  = "gentianos.io/gateway-component"
	gatewayComponentApp    = "app-route"
	gatewayComponentApex   = "apex-redirect"
	gatewayComponentKernel = "kernel-route"
)

type ingressIntent struct {
	appProfile string
	profile    *gentianov1alpha1.ComponentProfile
	ingress    *gentianov1alpha1.ExposureSpec
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
	ref := gatewayParentRef(AuthenticatedGatewayName)
	ns := gatewayv1.Namespace(servicesNamespace)
	ref.Namespace = &ns
	return ref
}

// tenantGatewayParentRefs attaches a tenant route to the kernel Gateway.
//
// kernelSectionName names the listener to bind. Pinning is required: an
// unpinned route attaches to every listener whose hostname matches, and the
// Gateway's plaintext :80 listener is hostname-less, so the route would outrank
// the redirect route there and serve the app over http.
func tenantGatewayParentRefs(kernelSectionName string) []gatewayv1.ParentReference {
	kernelRef := kernelGatewayParentRef()
	if kernelSectionName != "" {
		s := gatewayv1.SectionName(kernelSectionName)
		kernelRef.SectionName = &s
	}
	return []gatewayv1.ParentReference{kernelRef}
}

// pathMatch builds an HTTPRouteMatch of the given path-match type.
//
// pathExactMatch and pathPrefixMatch were separate copies in separate files,
// differing in one constant.
func pathMatch(t gatewayv1.PathMatchType, value string) gatewayv1.HTTPRouteMatch {
	return gatewayv1.HTTPRouteMatch{
		Path: &gatewayv1.HTTPPathMatch{
			Type:  &t,
			Value: &value,
		},
	}
}

func pathPrefixMatch(prefix string) gatewayv1.HTTPRouteMatch {
	return pathMatch(gatewayv1.PathMatchPathPrefix, prefix)
}

func appAPIBackendRules(
	profile *gentianov1alpha1.ComponentProfile,
	defaultPort int32,
	kernelDomain, effectiveDomain string,
	ingress *gentianov1alpha1.ExposureSpec,
) []gatewayv1.HTTPRouteRule {
	backends, err := gentianov1alpha1.ProfileGatewayAPIBackends(profile)
	if err != nil || len(backends) == 0 {
		return nil
	}
	mainIngressSubDomain := ""
	if gateways := profile.GatewayExposures(); len(gateways) > 0 {
		mainIngressSubDomain = gateways[0].SubDomain
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

func gatewayEmbeddingResponseFilters(
	kernelDomain, effectiveDomain, ingressSubDomain, mainIngressSubDomain string,
	ingress *gentianov1alpha1.ExposureSpec,
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

// consoleOrigins lists every desktop an app of one tenant may be framed by:
// the tenant's own console, and the platform's, whose tiles open a tenant's
// apps too. frame-ancestors is checked against the whole ancestor chain and
// the top frame is whichever console the user came from, so both are named.
//
// Both the computed default below and the "portal" token in
// ingressFrameAncestorsPolicy resolve through here. They used to enumerate the
// hosts separately, and the one route that opts out of the default (Collabora)
// lost the origin the user was actually on, and every document open failed
// with "Failed to load Nextcloud Office" while the server side stayed healthy.
// A console hostname must reach both policies at once.
func consoleOrigins(kernelDomain, effectiveDomain string) []string {
	var origins []string
	if kernelDomain != "" {
		origins = append(origins, "https://"+consoleHost(kernelDomain))
	}
	if effectiveDomain != "" && effectiveDomain != kernelDomain {
		origins = append(origins, "https://"+consoleHost(effectiveDomain))
	}
	return origins
}

func computeGatewayFrameAncestorsPolicy(kernelDomain, effectiveDomain, _ string) gatewayFrameAncestorsPolicy {
	origins := consoleOrigins(kernelDomain, effectiveDomain)
	if effectiveDomain != "" && effectiveDomain != kernelDomain {
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
