// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const (
	conditionIngressReady = "IngressReady"
	certManagerGroup      = "cert-manager.io"
	certManagerVersion    = "v1"
	certManagerCertKind   = "Certificate"

	// defaultDNS01ClusterIssuer is the cert-manager ClusterIssuer used for
	// per-tenant wildcard Certificates (*.<effectiveDomain>) when
	// TenantDNS01ClusterIssuer is unset. Override via TENANT_DNS01_CLUSTER_ISSUER
	// to match the cluster's DNS webhook (Cloudflare, Route53, Azure DNS, etc.).
	defaultDNS01ClusterIssuer = "letsencrypt-dns01-cloudflare"

	defaultServicePort  = int32(80)
	defaultIngressClass = "nginx"

	// kernelWildcardTenantSecret was replicated from the kernel wildcard into
	// tenant namespaces by older operator versions. Deleted on reconcile/delete
	// during migration to per-tenant wildcard certs.
	kernelWildcardTenantSecret = "kernel-wildcard-tls"
)

var certManagerCertGVK = schema.GroupVersionKind{
	Group:   certManagerGroup,
	Version: certManagerVersion,
	Kind:    certManagerCertKind,
}

// ensureIngress reconciles ingress + TLS for a tenant.
//
// Every tenant with ingress-enabled apps uses the same tenant-zone model:
//
//  1. effectiveDomain = Tenant.spec.domain if set, else "<tenant>.<kernel_domain>".
//  2. App hostnames are "<sub>.<effectiveDomain>" (e.g. meet.demo.desk.gentian.org).
//  3. One cert-manager Certificate per tenant for *.<effectiveDomain> and
//     <effectiveDomain>, stored as tenant-{name}-wildcard-tls (DNS-01).
//  4. All app Ingresses reference that secret.
//
// The kernel wildcard cert (*.<kernel_domain>) covers platform UIs only and is
// never replicated into tenant namespaces. See docs/design/multi-tenancy.md §3.
//
// When CloudflareDNS is configured, a proxied CNAME *.<effectiveDomain> is
// ensured so Total TLS (or equivalent) can mint edge certs for multi-level
// tenant hostnames. That adapter is optional and CSP-specific.
//
// IngressReady=True is reported once all required Ingress + Certificate
// resources have been applied. Cert-manager-side issuance status is tracked by
// the Certificate CRs themselves.
func (r *TenantReconciler) ensureIngress(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	nsName := tenantNamespaceName(tenant)

	type appIngress struct {
		appProfile string
		ingress    *gentianov1alpha1.IngressSpec
	}
	var ingressApps []appIngress

	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.Ingress != nil {
			ingressApps = append(ingressApps, appIngress{
				appProfile: app.Profile,
				ingress:    profile.Spec.Ingress,
			})
		}
		for i := range profile.Spec.AdditionalIngresses {
			ingressApps = append(ingressApps, appIngress{
				appProfile: additionalIngressProfile(app.Profile, i),
				ingress:    &profile.Spec.AdditionalIngresses[i],
			})
		}
	}

	if len(ingressApps) == 0 {
		if err := r.deleteStaleIngressesForTenant(ctx, tenant, nsName, nil); err != nil {
			return ctrl.Result{}, err
		}
		r.setCondition(tenant, conditionIngressReady, metav1.ConditionTrue,
			"NoIngressConfigured", "No apps require ingress provisioning")
		return ctrl.Result{}, nil
	}

	effectiveDomain := tenant.EffectiveDomain(r.KernelDomain)
	if effectiveDomain == "" {
		r.setCondition(tenant, conditionIngressReady, metav1.ConditionFalse,
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

	expectedIngresses := make(map[string]struct{}, len(ingressApps))
	for _, ia := range ingressApps {
		expectedIngresses[appIngressName(tenant.Name, ia.appProfile)] = struct{}{}
	}

	for _, ia := range ingressApps {
		host := ingressHost(ia.appProfile, ia.ingress, effectiveDomain)
		if err := r.ensureAppIngress(ctx, tenant, nsName, ia.appProfile, ia.ingress, host, tlsSecret, effectiveDomain); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Ingress for app %s: %w", ia.appProfile, err)
		}
	}

	if err := r.deleteStaleIngressesForTenant(ctx, tenant, nsName, expectedIngresses); err != nil {
		return ctrl.Result{}, err
	}

	r.setCondition(tenant, conditionIngressReady, metav1.ConditionTrue,
		"Provisioned",
		fmt.Sprintf("Ingress provisioned for %d app(s) on %q (tenant-zone-wildcard)", len(ingressApps), effectiveDomain))
	return ctrl.Result{}, nil
}

// tenantDNS01ClusterIssuer returns the ClusterIssuer for per-tenant wildcard certs.
func (r *TenantReconciler) tenantDNS01ClusterIssuer() string {
	if r.TenantDNS01ClusterIssuer != "" {
		return r.TenantDNS01ClusterIssuer
	}
	return defaultDNS01ClusterIssuer
}

// ensureTenantWildcardEdgeDNS creates optional CSP edge DNS (e.g. Cloudflare
// Total TLS) for *.<effectiveDomain>. Failures are logged and non-fatal.
func (r *TenantReconciler) ensureTenantWildcardEdgeDNS(ctx context.Context, effectiveDomain string) {
	if r.CloudflareDNS == nil {
		return
	}
	wildcard := "*." + effectiveDomain
	if err := r.CloudflareDNS.ensureCNAME(ctx, wildcard, r.CloudflareDNS.tunnelCNAME); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "ensure Cloudflare wildcard DNS CNAME", "host", wildcard)
	}
}

// deleteLegacyKernelWildcardSecret removes the replicated kernel wildcard secret
// left by older operator versions.
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

// deleteStaleIngressesForTenant removes any operator-managed ingresses in the
// tenant namespace whose names are NOT in the expectedIngresses set.  Pass nil
// for expectedIngresses to delete ALL operator-managed ingresses (used during
// full app removal).
func (r *TenantReconciler) deleteStaleIngressesForTenant(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	expectedIngresses map[string]struct{},
) error {
	list := &networkingv1.IngressList{}
	if err := r.List(ctx, list,
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue, tenantLabel: tenant.Name},
	); err != nil {
		return fmt.Errorf("list tenant ingresses for stale cleanup: %w", err)
	}
	for i := range list.Items {
		name := list.Items[i].Name
		if isUMCFrontendIngress(&list.Items[i]) {
			continue
		}
		if expectedIngresses != nil {
			if _, wanted := expectedIngresses[name]; wanted {
				continue
			}
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale ingress %s: %w", name, err)
		}
		ctrl.LoggerFrom(ctx).Info("deleted stale tenant ingress", "ingress", name, "tenant", tenant.Name)
	}
	return nil
}

// ensureTenantWildcardCertificate creates a wildcard cert-manager Certificate
// CR for *.<domain> (and <domain>) in the tenant namespace via DNS-01.
// Idempotent: existence is taken as up-to-date.
func (r *TenantReconciler) ensureTenantWildcardCertificate(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName, certName, secretName, domain string,
) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(certManagerCertGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: certName, Namespace: nsName}, existing); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		desired := buildTenantWildcardCertificate(tenant, nsName, certName, secretName, domain, r.tenantDNS01ClusterIssuer())
		return r.Create(ctx, desired)
	}
	return nil
}

func (r *TenantReconciler) ensureAppIngress(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName, appProfile string,
	ingress *gentianov1alpha1.IngressSpec,
	host, tlsSecret string,
	effectiveDomain string,
) error {
	desired := buildAppIngress(tenant, nsName, appProfile, ingress, host, tlsSecret, effectiveDomain)
	name := appIngressName(tenant.Name, appProfile)

	existing := &networkingv1.Ingress{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, existing); err != nil {
		if errors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) ||
		!equality.Semantic.DeepEqual(existing.Annotations, desired.Annotations) {
		existing.Spec = desired.Spec
		existing.Annotations = desired.Annotations
		return r.Update(ctx, existing)
	}
	return nil
}

func (r *TenantReconciler) deleteIngress(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	nsName := tenantNamespaceName(tenant)

	effectiveDomain := tenant.EffectiveDomain(r.KernelDomain)
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.Ingress == nil {
			continue
		}
		ing := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:      appIngressName(tenant.Name, app.Profile),
				Namespace: nsName,
			},
		}
		if err := r.Delete(ctx, ing); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Ingress for app %s: %w", app.Profile, err)
		}
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
			ctrl.LoggerFrom(ctx).Error(err, "delete Cloudflare wildcard DNS CNAME", "host", wildcard)
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

func buildAppIngress(
	tenant *gentianov1alpha1.Tenant,
	nsName, appProfile string,
	ingress *gentianov1alpha1.IngressSpec,
	host, tlsSecret string,
	effectiveDomain string,
) *networkingv1.Ingress {
	svcName := ingress.ServiceName
	if svcName == "" {
		svcName = appProfile
	}
	svcPort := ingress.ServicePort
	if svcPort == 0 {
		svcPort = defaultServicePort
	}
	annotations := map[string]string{
		managedByLabel: managedByValue,
	}
	for k, v := range ingress.Annotations {
		annotations[k] = strings.ReplaceAll(v, "${TENANT_DOMAIN}", effectiveDomain)
	}
	ingressClass := ingress.IngressClassName
	if ingressClass == "" {
		ingressClass = defaultIngressClass
	}
	pathType := networkingv1.PathTypePrefix
	obj := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appIngressName(tenant.Name, appProfile),
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				appLabel:       appProfile,
				managedByLabel: managedByValue,
			},
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			Rules: []networkingv1.IngressRule{
				{
					Host: host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: svcName,
											Port: networkingv1.ServiceBackendPort{
												Number: svcPort,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	if ingress.TLSEnabled {
		obj.Spec.TLS = []networkingv1.IngressTLS{
			{
				Hosts:      []string{host},
				SecretName: tlsSecret,
			},
		}
	}
	return obj
}

func tenantWildcardCertName(tenantName string) string {
	return fmt.Sprintf("tenant-%s-wildcard", tenantName)
}

func tenantWildcardSecretName(tenantName string) string {
	return fmt.Sprintf("tenant-%s-wildcard-tls", tenantName)
}

func appIngressName(tenantName, appProfile string) string {
	return fmt.Sprintf("ingress-%s-%s", tenantName, appProfile)
}

// additionalIngressProfile returns a synthetic profile key used as the ingress
// name component for AdditionalIngresses entries. The index suffix ensures
// each additional ingress has a unique, stable name.
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
