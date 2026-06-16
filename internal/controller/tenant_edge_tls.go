// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	defaultDNS01ClusterIssuer = "letsencrypt-dns01-cloudflare"
	defaultServicePort        = int32(80)

	// kernelWildcardTenantSecret was replicated from the kernel wildcard into
	// tenant namespaces by older operator versions.
	kernelWildcardTenantSecret = "kernel-wildcard-tls"
)

var certManagerCertGVK = schema.GroupVersionKind{
	Group:   "cert-manager.io",
	Version: "v1",
	Kind:    "Certificate",
}

func (r *TenantReconciler) tenantDNS01ClusterIssuer() string {
	if r.TenantDNS01ClusterIssuer != "" {
		return r.TenantDNS01ClusterIssuer
	}
	return defaultDNS01ClusterIssuer
}

func buildTenantWildcardCertificate(
	tenant *gentianov1alpha1.Tenant,
	nsName, certName, secretName, domain, clusterIssuer string,
) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(certManagerCertGVK)
	obj.SetName(certName)
	obj.SetNamespace(nsName)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	_ = unstructured.SetNestedStringSlice(obj.Object, []string{"*." + domain, domain}, "spec", "dnsNames")
	_ = unstructured.SetNestedField(obj.Object, secretName, "spec", "secretName")
	_ = unstructured.SetNestedField(obj.Object, map[string]interface{}{
		"name": clusterIssuer,
		"kind": "ClusterIssuer",
	}, "spec", "issuerRef")
	return obj
}

func (r *TenantReconciler) deleteLegacyKernelWildcardSecret(ctx context.Context, nsName string) error {
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kernelWildcardTenantSecret,
			Namespace: nsName,
		},
	}
	if err := r.Delete(ctx, legacy); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete legacy kernel wildcard TLS secret: %w", err)
	}
	return nil
}

func (r *TenantReconciler) ensureTenantWildcardEdgeDNS(ctx context.Context, tenant *gentianov1alpha1.Tenant, effectiveDomain string) {
	if r.CloudflareDNS == nil {
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	wildcard := "*." + effectiveDomain
	if err := r.CloudflareDNS.ensureCNAME(ctx, wildcard, r.CloudflareDNS.tunnelCNAME); err != nil {
		logger.Error(err, "ensure Cloudflare wildcard DNS CNAME", "host", wildcard)
	}
	if err := r.CloudflareDNS.ensureCNAME(ctx, effectiveDomain, r.CloudflareDNS.tunnelCNAME); err != nil {
		logger.Error(err, "ensure Cloudflare apex DNS CNAME", "host", effectiveDomain)
	}
	origin, err := kernelGatewayTunnelOrigin(ctx, r.Client)
	if err != nil {
		logger.Error(err, "resolve kernel gateway tunnel origin")
		r.setCondition(tenant, conditionTunnelIngressReady, metav1.ConditionFalse, "OriginLookupFailed", err.Error())
		return
	}
	tunnelOK := true
	var tunnelMsg string
	for _, host := range []string{wildcard, effectiveDomain} {
		if err := r.CloudflareDNS.ensureTunnelIngress(ctx, host, origin); err != nil {
			logger.Error(err, "ensure Cloudflare tunnel ingress", "host", host, "origin", origin)
			tunnelOK = false
			tunnelMsg = err.Error()
		}
	}
	if tunnelOK {
		r.setCondition(tenant, conditionTunnelIngressReady, metav1.ConditionTrue, "Programmed",
			fmt.Sprintf("Cloudflare tunnel ingress configured for %q → %s", wildcard, origin))
	} else {
		r.setCondition(tenant, conditionTunnelIngressReady, metav1.ConditionFalse, "CloudflareTunnelSyncFailed", tunnelMsg)
	}
}

func (r *TenantReconciler) deleteEdgeRouting(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	nsName := tenantNamespaceName(tenant)
	effectiveDomain := r.tenantEffectiveDomain(tenant)
	logger := ctrl.LoggerFrom(ctx)

	if err := r.deleteTenantGateway(ctx, nsName, tenant.Name); err != nil {
		return err
	}
	if err := r.deleteTenantHTTPRoutes(ctx, tenant, nsName); err != nil {
		return err
	}

	wildcardCert := &unstructured.Unstructured{}
	wildcardCert.SetGroupVersionKind(certManagerCertGVK)
	wildcardCert.SetName(tenantWildcardCertName(tenant.Name))
	wildcardCert.SetNamespace(nsName)
	if err := r.Delete(ctx, wildcardCert); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete tenant wildcard Certificate: %w", err)
	}

	if effectiveDomain != "" && r.CloudflareDNS != nil {
		wildcard := "*." + effectiveDomain
		if err := r.CloudflareDNS.deleteCNAME(ctx, wildcard); err != nil {
			logger.Error(err, "delete Cloudflare wildcard DNS CNAME", "host", wildcard)
		}
		for _, host := range []string{wildcard, effectiveDomain} {
			if err := r.CloudflareDNS.deleteTunnelIngress(ctx, host); err != nil {
				logger.Error(err, "delete Cloudflare tunnel ingress", "host", host)
			}
		}
	}

	if err := r.deleteLegacyKernelWildcardSecret(ctx, nsName); err != nil {
		return err
	}

	tenantWildcardTLS := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantWildcardSecretName(tenant.Name),
			Namespace: nsName,
		},
	}
	if err := r.Delete(ctx, tenantWildcardTLS); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete tenant wildcard TLS secret: %w", err)
	}
	return nil
}

func tenantWildcardCertName(tenantName string) string {
	return fmt.Sprintf("tenant-%s-wildcard", tenantName)
}

func tenantWildcardSecretName(tenantName string) string {
	return fmt.Sprintf("tenant-%s-wildcard-tls", tenantName)
}

func additionalIngressProfile(appProfile string, index int) string {
	return fmt.Sprintf("%s-extra%d", appProfile, index)
}

func ingressHost(appProfile string, ingress *gentianov1alpha1.IngressSpec, effectiveDomain string) string {
	sub := ingress.SubDomain
	if sub == "" {
		sub = appProfile
	}
	return fmt.Sprintf("%s.%s", sub, effectiveDomain)
}

func isUMCFrontendIngress(ing *networkingv1.Ingress) bool {
	return ing.Labels[umcFrontendComponentLabel] == umcFrontendComponentValue
}
