// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ensureGateway reconciles Gateway API edge resources for a tenant when
// ROUTING_MODE=gateway: tenant Gateway, wildcard TLS, HTTPRoutes, and policies.
func (r *TenantReconciler) ensureGateway(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	if !isGatewayRoutingMode(r.RoutingMode) {
		return ctrl.Result{}, nil
	}

	nsName := tenantNamespaceName(tenant)
	intents, err := r.collectTenantIngressIntents(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(intents) == 0 {
		if err := r.deleteTenantGateway(ctx, nsName, tenant.Name); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.deleteTenantHTTPRoutes(ctx, tenant, nsName); err != nil {
			return ctrl.Result{}, err
		}
		r.setCondition(tenant, conditionGatewayReady, metav1.ConditionTrue,
			"NoGatewayConfigured", "No apps require gateway provisioning")
		return ctrl.Result{}, nil
	}

	effectiveDomain := r.tenantEffectiveDomain(tenant)
	if effectiveDomain == "" {
		r.setCondition(tenant, conditionGatewayReady, metav1.ConditionFalse,
			"NoDomain", "tenant.spec.domain is unset and operator KERNEL_DOMAIN is not configured")
		return ctrl.Result{}, fmt.Errorf("no effective domain available for tenant %s", tenant.Name)
	}

	wildcardCertName := tenantWildcardCertName(tenant.Name)
	tlsSecret := tenantWildcardSecretName(tenant.Name)
	if err := r.ensureTenantWildcardCertificate(ctx, tenant, nsName, wildcardCertName, tlsSecret, effectiveDomain); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure tenant wildcard certificate: %w", err)
	}
	if err := r.deleteLegacyKernelWildcardSecret(ctx, nsName); err != nil {
		return ctrl.Result{}, err
	}
	r.ensureTenantWildcardEdgeDNS(ctx, effectiveDomain)

	desiredGW := buildTenantGateway(tenant, nsName, effectiveDomain, tlsSecret)
	if err := ensureGatewayResource(ctx, r.Client, desiredGW); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure tenant Gateway: %w", err)
	}
	if err := ensureTenantKernelGatewayReferenceGrants(ctx, r.Client, tenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure kernel Gateway ReferenceGrants: %w", err)
	}

	expectedRoutes := make(map[string]struct{}, len(intents))
	allRoutesReady := true
	var notReadyReason string

	for _, intent := range intents {
		host := ingressHost(intent.appProfile, intent.ingress, effectiveDomain)
		route := buildAppHTTPRoute(tenant, nsName, intent.appProfile, intent.ingress, host, effectiveDomain, r.KernelDomain)
		expectedRoutes[route.Name] = struct{}{}
		if err := r.ensureHTTPRouteResource(ctx, route); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure HTTPRoute for app %s: %w", intent.appProfile, err)
		}
		if err := r.ensureAppBackendTrafficPolicyWithRoute(ctx, tenant, nsName, intent.appProfile, intent.ingress); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure BackendTrafficPolicy for app %s: %w", intent.appProfile, err)
		}
		if ok, reason := httpRouteProgrammed(ctx, r.Client, route); !ok {
			allRoutesReady = false
			notReadyReason = reason
		}
	}

	if err := r.deleteStaleHTTPRoutesForTenant(ctx, tenant, nsName, expectedRoutes); err != nil {
		return ctrl.Result{}, err
	}

	message := fmt.Sprintf("Gateway provisioned for %d app(s) on %q (tenant-zone-wildcard)", len(intents), effectiveDomain)
	if programmed, reason := gatewayProgrammed(ctx, r.Client, desiredGW); !programmed {
		r.setCondition(tenant, conditionGatewayReady, metav1.ConditionFalse, reason, message)
		return ctrl.Result{RequeueAfter: requeueGatewayAfter}, nil
	}
	if !allRoutesReady {
		if notReadyReason == "" {
			notReadyReason = "AwaitingHTTPRoutes"
		}
		r.setCondition(tenant, conditionGatewayReady, metav1.ConditionFalse, notReadyReason, message)
		return ctrl.Result{RequeueAfter: requeueGatewayAfter}, nil
	}

	r.setCondition(tenant, conditionGatewayReady, metav1.ConditionTrue, "Programmed", message)
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) deleteTenantGateway(ctx context.Context, nsName, tenantName string) error {
	gw := &gatewayv1.Gateway{}
	err := r.Get(ctx, client.ObjectKey{Name: tenantGatewayName(tenantName), Namespace: nsName}, gw)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	prop := metav1.DeletePropagationBackground
	return r.Delete(ctx, gw, &client.DeleteOptions{PropagationPolicy: &prop})
}

func gatewayProgrammed(ctx context.Context, c client.Client, gw *gatewayv1.Gateway) (bool, string) {
	current := &gatewayv1.Gateway{}
	if err := c.Get(ctx, client.ObjectKey{Name: gw.Name, Namespace: gw.Namespace}, current); err != nil {
		return false, "GatewayMissing"
	}
	for _, cond := range current.Status.Conditions {
		if cond.Type == string(gatewayv1.GatewayConditionProgrammed) {
			if cond.Status == metav1.ConditionTrue {
				return true, cond.Reason
			}
			reason := cond.Reason
			if reason == "" {
				reason = "NotProgrammed"
			}
			return false, reason
		}
	}
	return false, "AwaitingProgrammed"
}
