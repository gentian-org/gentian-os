// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ensureGateway handles operator-only gateway edge work when ROUTING_MODE=gateway:
// Cloudflare DNS, stale route/policy cleanup, legacy Ingress removal, and readiness waits.
// Kubernetes edge objects (Gateway, HTTPRoutes, ReferenceGrants, BackendTrafficPolicy)
// are owned by Crossplane via the manifest bridge.
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
		if err := deleteTenantReferenceGrants(ctx, r.Client, tenant); err != nil {
			return ctrl.Result{}, fmt.Errorf("delete tenant ReferenceGrants: %w", err)
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

	if err := r.deleteLegacyKernelWildcardSecret(ctx, nsName); err != nil {
		return ctrl.Result{}, err
	}
	r.ensureTenantWildcardEdgeDNS(ctx, tenant, effectiveDomain)

	expectedRoutes := make(map[string]struct{}, len(intents))
	expectedPolicies := make(map[string]struct{})
	for _, intent := range intents {
		route := buildAppHTTPRoute(tenant, nsName, intent.appProfile, intent.profile, intent.ingress,
			ingressHost(intent.appProfile, intent.ingress, effectiveDomain), effectiveDomain, r.KernelDomain)
		expectedRoutes[route.Name] = struct{}{}
		if btp := buildAppBackendTrafficPolicyObject(tenant, nsName, intent.appProfile, intent.ingress); btp != nil {
			expectedPolicies[btp.GetName()] = struct{}{}
		}
	}

	if err := r.deleteStaleHTTPRoutesForTenant(ctx, tenant, nsName, expectedRoutes); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteStaleBackendTrafficPoliciesForTenant(ctx, tenant, nsName, expectedPolicies); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteSupersededTenantIngress(ctx, tenant, nsName, intents, effectiveDomain); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete superseded tenant Ingress: %w", err)
	}

	message := fmt.Sprintf("Gateway provisioned for %d app(s) on %q (tenant-zone-wildcard)", len(intents), effectiveDomain)
	ready, reason, err := r.waitForTenantEdgeResources(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		if reason == "" {
			reason = "AwaitingEdgeResources"
		}
		r.setCondition(tenant, conditionGatewayReady, metav1.ConditionFalse, reason, message)
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
			// ClusterIP/tunnel clusters may never assign a Gateway address; listeners
			// can still be programmed and serve traffic via tunnel or in-cluster paths.
			if cond.Reason == "AddressNotAssigned" && gatewayListenersProgrammed(current) {
				return true, "ListenersProgrammed"
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

func gatewayListenersProgrammed(gw *gatewayv1.Gateway) bool {
	if len(gw.Status.Listeners) == 0 {
		return false
	}
	for _, listener := range gw.Status.Listeners {
		programmed := false
		for _, cond := range listener.Conditions {
			if cond.Type == string(gatewayv1.GatewayConditionProgrammed) &&
				cond.Status == metav1.ConditionTrue {
				programmed = true
				break
			}
		}
		if !programmed {
			return false
		}
	}
	return true
}

// deleteSupersededTenantIngress removes chart- or legacy-managed Ingress objects
// whose hostnames are now served by operator-managed HTTPRoutes.
func (r *TenantReconciler) deleteSupersededTenantIngress(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	intents []ingressIntent,
	effectiveDomain string,
) error {
	if len(intents) == 0 || effectiveDomain == "" {
		return nil
	}

	hosts := make(map[string]struct{}, len(intents))
	for _, intent := range intents {
		host := ingressHost(intent.appProfile, intent.ingress, effectiveDomain)
		if host != "" {
			hosts[strings.ToLower(host)] = struct{}{}
		}
	}

	list := &networkingv1.IngressList{}
	if err := r.List(ctx, list, client.InNamespace(nsName)); err != nil {
		return fmt.Errorf("list tenant Ingress resources: %w", err)
	}

	logger := ctrl.LoggerFrom(ctx)
	for i := range list.Items {
		ing := &list.Items[i]
		if isPortalRedirectIngress(ing) {
			continue
		}
		if !tenantIngressSupersededByGateway(ing, hosts) {
			continue
		}
		if err := r.Delete(ctx, ing); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete superseded tenant Ingress %s: %w", ing.Name, err)
		}
		logger.Info("deleted legacy tenant Ingress superseded by Gateway API",
			"ingress", ing.Name, "tenant", tenant.Name)
	}
	return nil
}

func tenantIngressSupersededByGateway(ing *networkingv1.Ingress, hosts map[string]struct{}) bool {
	for _, rule := range ing.Spec.Rules {
		if _, ok := hosts[strings.ToLower(rule.Host)]; ok {
			return true
		}
	}
	return false
}
