// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// keycloakProxyIngressName is the Nubus Keycloak extensions proxy Ingress that
// serves id.<kernel-domain>. Override via KEYCLOAK_PROXY_INGRESS_NAME per env.
func keycloakProxyIngressName() string {
	return envOrDefault("KEYCLOAK_PROXY_INGRESS_NAME", "nubus-dev-keycloak-extensions-proxy")
}

// ensureKeycloakIDPEmbeddingIngress patches the shared Keycloak ingress so
// portal-embedded tenant apps (chat.<tenant>.<kernel>, etc.) may frame OIDC pages.
// Baseline CSP also lives in nubus Helm values (kernel/services/nubus/manifests);
// the operator converges the annotation when tenants are added or removed.
func (r *TenantReconciler) ensureKeycloakIDPEmbeddingIngress(ctx context.Context) error {
	if r.KernelDomain == "" {
		return nil
	}

	tenantList := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenantList); err != nil {
		return fmt.Errorf("list tenants for Keycloak IdP CSP: %w", err)
	}

	var effectiveDomains []string
	for i := range tenantList.Items {
		if tenantList.Items[i].DeletionTimestamp != nil {
			continue
		}
		if d := r.tenantEffectiveDomain(&tenantList.Items[i]); d != "" {
			effectiveDomains = append(effectiveDomains, d)
		}
	}

	desiredSnippet := keycloakOIDCEmbeddingIngressSnippet(r.KernelDomain, effectiveDomains)
	if desiredSnippet == "" {
		return nil
	}

	name := keycloakProxyIngressName()
	existing := &networkingv1.Ingress{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: servicesNamespace}, existing)
	if errors.IsNotFound(err) {
		log.FromContext(ctx).Info("Keycloak proxy ingress not found; IdP frame-ancestors will apply on next Nubus deploy",
			"ingress", name, "namespace", servicesNamespace)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Keycloak proxy ingress %s/%s: %w", servicesNamespace, name, err)
	}

	annotations := make(map[string]string, len(existing.Annotations)+1)
	for k, v := range existing.Annotations {
		annotations[k] = v
	}
	if annotations[nginxConfigurationSnippetAnnotation] == desiredSnippet {
		return nil
	}
	annotations[nginxConfigurationSnippetAnnotation] = desiredSnippet

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Annotations = annotations
	if err := r.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("patch Keycloak proxy ingress %s/%s frame-ancestors: %w", servicesNamespace, name, err)
	}
	log.FromContext(ctx).Info("updated Keycloak IdP ingress frame-ancestors for portal-embedded SSO",
		"ingress", name, "namespace", servicesNamespace, "tenantZones", len(effectiveDomains))
	return nil
}
