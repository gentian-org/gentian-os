// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func reconcileKeycloakIDPGatewayRoute(ctx context.Context, c client.Client, kernelDomain, tenancyMode string) error {
	if kernelDomain == "" {
		return nil
	}

	tenantList := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for Keycloak IdP gateway route: %w", err)
	}

	var effectiveDomains []string
	var tenantNames []string
	for i := range tenantList.Items {
		if tenantList.Items[i].DeletionTimestamp != nil {
			continue
		}
		if d := tenantList.Items[i].EffectiveDomain(kernelDomain, tenancyMode); d != "" {
			effectiveDomains = append(effectiveDomains, d)
			tenantNames = append(tenantNames, tenantList.Items[i].Name)
		}
	}
	oidcSubs, err := collectOIDCIngressSubdomainsByTenant(ctx, c, tenantList.Items)
	if err != nil {
		return fmt.Errorf("collect OIDC ingress subdomains: %w", err)
	}

	desiredFilters := keycloakGatewayResponseFilters(kernelDomain, effectiveDomains, oidcSubs, tenantNames)
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
		return fmt.Errorf("Keycloak IdP HTTPRoute %s has no rules", existing.Name)
	}

	desiredRule := existing.Spec.Rules[0]
	desiredRule.Filters = desiredFilters
	desiredSpec := existing.Spec.DeepCopy()
	desiredSpec.Rules = []gatewayv1.HTTPRouteRule{desiredRule}

	if equality.Semantic.DeepEqual(existing.Spec, *desiredSpec) {
		return nil
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec = *desiredSpec
	if err := c.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("patch Keycloak IdP HTTPRoute frame-ancestors: %w", err)
	}
	log.FromContext(ctx).Info("updated Keycloak IdP HTTPRoute frame-ancestors for portal-embedded SSO",
		"route", existing.Name, "namespace", servicesNamespace, "tenantZones", len(effectiveDomains))
	return nil
}

func keycloakGatewayFramePolicyApplied(ctx context.Context, c client.Client, kernelDomain, tenancyMode string) bool {
	tenantList := &gentianov1alpha1.TenantList{}
	if err := c.List(ctx, tenantList); err != nil {
		return false
	}
	var effectiveDomains []string
	var tenantNames []string
	for i := range tenantList.Items {
		if tenantList.Items[i].DeletionTimestamp != nil {
			continue
		}
		if d := tenantList.Items[i].EffectiveDomain(kernelDomain, tenancyMode); d != "" {
			effectiveDomains = append(effectiveDomains, d)
			tenantNames = append(tenantNames, tenantList.Items[i].Name)
		}
	}
	oidcSubs, err := collectOIDCIngressSubdomainsByTenant(ctx, c, tenantList.Items)
	if err != nil {
		return false
	}
	desiredFilters := keycloakGatewayResponseFilters(kernelDomain, effectiveDomains, oidcSubs, tenantNames)
	if len(desiredFilters) == 0 {
		return true
	}

	existing := &gatewayv1.HTTPRoute{}
	if err = c.Get(ctx, client.ObjectKey{Name: kernelKeycloakHTTPRouteName(), Namespace: servicesNamespace}, existing); err != nil {
		return false
	}
	if len(existing.Spec.Rules) == 0 || len(existing.Spec.Rules[0].Filters) == 0 {
		return false
	}
	modifier := existing.Spec.Rules[0].Filters[0].ResponseHeaderModifier
	if modifier == nil {
		return false
	}
	var csp string
	for _, h := range modifier.Set {
		if h.Name == "Content-Security-Policy" {
			csp = h.Value
			break
		}
	}
	if csp == "" {
		return false
	}
	if !strings.Contains(csp, fmt.Sprintf("https://portal.%s", kernelDomain)) {
		return false
	}
	if !strings.Contains(csp, fmt.Sprintf("https://*.%s", kernelDomain)) {
		return false
	}
	for _, d := range effectiveDomains {
		if !strings.Contains(csp, fmt.Sprintf("https://*.%s", d)) {
			return false
		}
	}
	for tenantName, subs := range oidcSubs {
		effective := ""
		for i, name := range tenantNames {
			if name == tenantName && i < len(effectiveDomains) {
				effective = effectiveDomains[i]
				break
			}
		}
		if effective == "" {
			continue
		}
		for _, sub := range subs {
			origin := fmt.Sprintf("https://%s.%s", sub, effective)
			if !strings.Contains(csp, origin) {
				return false
			}
		}
	}
	return true
}
