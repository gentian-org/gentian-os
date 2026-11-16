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

// ingressIntent is one gateway entry of a tenant's installed app, and the
// component whose name its host is formed from (addresses.Host).
type ingressIntent struct {
	component string
	profile   *gentianov1alpha1.ComponentProfile
	ingress   *gentianov1alpha1.ExposureSpec
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

// keycloakGatewayResponseFilters is the identity provider's frame policy:
// its own pages and the signInFramers may frame it, nobody else.
func keycloakGatewayResponseFilters(framers []string) []gatewayv1.HTTPRouteFilter {
	var sources []string
	for _, h := range framers {
		if h != "" {
			sources = append(sources, "https://"+h)
		}
	}
	if len(sources) == 0 {
		return nil
	}
	origins := strings.Join(sources, " ")
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
