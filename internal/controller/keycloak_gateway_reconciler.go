/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func reconcileKeycloakIDPGatewayRoute(ctx context.Context, c client.Client, kernelDomain, tenancyMode, kernelRealm string) error {
	if kernelDomain == "" {
		return nil
	}

	tenantList := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for Keycloak IdP gateway route: %w", err)
	}

	framers, err := signInFramers(ctx, c, tenantList.Items, kernelDomain, tenancyMode, kernelRealm)
	if err != nil {
		return fmt.Errorf("collect the pages that may frame sign-in: %w", err)
	}

	desiredFilters := keycloakGatewayResponseFilters(framers)
	if len(desiredFilters) == 0 {
		return nil
	}

	existing := &gatewayv1.HTTPRoute{}
	err = c.Get(ctx, client.ObjectKey{Name: kernelKeycloakHTTPRouteName(), Namespace: servicesNamespace}, existing)
	if errors.IsNotFound(err) {
		log.FromContext(ctx).Info("Keycloak IdP HTTPRoute not found; frame-ancestors will apply when kernel routes are ready",
			"route", kernelKeycloakHTTPRouteName(), "namespace", servicesNamespace)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Keycloak IdP HTTPRoute: %w", err)
	}
	if len(existing.Spec.Rules) == 0 {
		return fmt.Errorf("keycloak IdP HTTPRoute %s has no rules", existing.Name)
	}

	// The filters on every rule, the rules themselves untouched: the route
	// is the kernel route builder's, and it carries one rule per allowed
	// prefix. Rebuilding it from rule zero alone collapsed it to a single
	// prefix every time this ran after the builder, and the two writers
	// took turns -- id.<kernel> answered 404 on whichever prefix had just
	// been dropped.
	desiredSpec := existing.Spec.DeepCopy()
	for i := range desiredSpec.Rules {
		desiredSpec.Rules[i].Filters = desiredFilters
	}

	if equality.Semantic.DeepEqual(existing.Spec, *desiredSpec) {
		return nil
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec = *desiredSpec
	if err := c.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("patch Keycloak IdP HTTPRoute frame-ancestors: %w", err)
	}
	log.FromContext(ctx).Info("updated Keycloak IdP HTTPRoute frame-ancestors",
		"route", existing.Name, "namespace", servicesNamespace, "framers", len(framers))
	return nil
}

func keycloakGatewayFramePolicyApplied(ctx context.Context, c client.Client, kernelDomain, tenancyMode, kernelRealm string) bool {
	tenantList := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenantList); err != nil {
		return false
	}
	framers, err := signInFramers(ctx, c, tenantList.Items, kernelDomain, tenancyMode, kernelRealm)
	if err != nil {
		return false
	}
	desiredFilters := keycloakGatewayResponseFilters(framers)
	if len(desiredFilters) == 0 {
		return true
	}

	existing := &gatewayv1.HTTPRoute{}
	if err = c.Get(ctx, client.ObjectKey{Name: kernelKeycloakHTTPRouteName(), Namespace: servicesNamespace}, existing); err != nil {
		return false
	}
	if len(existing.Spec.Rules) == 0 {
		return false
	}
	// Every rule carries the policy, with exactly the sources wanted: one
	// source more is a page the realm may be framed by that nobody allowed.
	want, _ := frameAncestors(contentSecurityPolicy(desiredFilters))
	for _, rule := range existing.Spec.Rules {
		got, ok := frameAncestors(contentSecurityPolicy(rule.Filters))
		if !ok || !sameSources(got, want) {
			return false
		}
	}
	return true
}

// contentSecurityPolicy is the Content-Security-Policy the filters set on a
// response, or "" where they set none.
func contentSecurityPolicy(filters []gatewayv1.HTTPRouteFilter) string {
	for _, f := range filters {
		if f.ResponseHeaderModifier == nil {
			continue
		}
		for _, h := range f.ResponseHeaderModifier.Set {
			if strings.EqualFold(string(h.Name), "Content-Security-Policy") {
				return h.Value
			}
		}
	}
	return ""
}

// frameAncestors is the source list of a policy's frame-ancestors directive,
// and whether it has one. Directives are separated by semicolons and named by
// their first token, case-insensitively; a browser obeys the first of a name
// and ignores the rest.
func frameAncestors(policy string) ([]string, bool) {
	for _, directive := range strings.Split(policy, ";") {
		tokens := strings.Fields(directive)
		if len(tokens) > 0 && strings.EqualFold(tokens[0], "frame-ancestors") {
			return tokens[1:], true
		}
	}
	return nil, false
}

// sameSources reports two source lists that allow the same sources, in
// whatever order.
func sameSources(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := make(map[string]int, len(a))
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		if count[s] == 0 {
			return false
		}
		count[s]--
	}
	return true
}
