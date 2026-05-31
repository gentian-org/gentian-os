// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

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

	// defaultDNS01ClusterIssuer is used to issue a single per-tenant wildcard
	// Certificate (*.<effectiveDomain>) that covers all app subdomains. Using
	// DNS-01 avoids HTTP-01 per-host rate limits and handles multi-level
	// subdomains (e.g. *.gtn-demo-2.desk.gentian.org) that are not covered by
	// Cloudflare's Universal SSL single-level wildcard.
	defaultDNS01ClusterIssuer = "letsencrypt-dns01-cloudflare"

	defaultServicePort  = int32(80)
	defaultIngressClass = "nginx"

	// Kernel wildcard TLS Secret coordinates. Created at install time by
	// kernel/manifests/cert-manager/wildcard-kernel.yaml and replicated by
	// the operator into each tenant namespace under a fixed name.
	kernelWildcardSourceNamespace = "cert-manager"
	kernelWildcardSourceSecret    = "wildcard-kernel-tls"
	kernelWildcardTenantSecret    = "kernel-wildcard-tls"
)

var certManagerCertGVK = schema.GroupVersionKind{
	Group:   certManagerGroup,
	Version: certManagerVersion,
	Kind:    certManagerCertKind,
}

// ensureIngress reconciles ingress + TLS for a tenant.
//
// There are two modes, selected by Tenant.HasVanityDomain():
//
//  1. Fallback mode (no vanity domain): hosts are
//     "<sub>.<tenant>.<kernel_domain>", served under the cluster-wide
//     kernel wildcard certificate. The operator replicates the kernel TLS
//     Secret from the cert-manager namespace into the tenant namespace and
//     each app Ingress references it.
//
//  2. Vanity mode (Tenant.spec.domain set): hosts are
//     "<sub>.<vanity_domain>". The operator creates a single wildcard
//     cert-manager Certificate CR for *.<vanity_domain> using DNS-01
//     (Cloudflare). This single cert covers all per-tenant app subdomains,
//     avoids HTTP-01 per-host rate limits, and works with multi-level
//     subdomains not covered by Cloudflare Universal SSL.
//
// IngressReady=True is reported once all required Ingress + Certificate
// resources have been applied. Cert-manager-side issuance status is
// tracked by the Certificate CRs themselves.
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
		// Still clean up any stale ingresses from previously removed apps.
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

	vanity := tenant.HasVanityDomain()

	if !vanity {
		if err := r.ensureKernelWildcardSecret(ctx, tenant, nsName); err != nil {
			return ctrl.Result{}, fmt.Errorf("replicate kernel wildcard TLS secret: %w", err)
		}
	}

	tlsSecret := kernelWildcardTenantSecret
	if vanity {
		// Issue one wildcard cert for *.<effectiveDomain> that covers all app
		// subdomains. DNS-01 supports multi-level wildcards and does not exhaust
		// per-identifier HTTP-01 rate limits.
		wildcardCertName := tenantWildcardCertName(tenant.Name)
		tlsSecret = tenantWildcardSecretName(tenant.Name)
		if err := r.ensureTenantWildcardCertificate(ctx, tenant, nsName, wildcardCertName, tlsSecret, effectiveDomain); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure tenant wildcard certificate: %w", err)
		}
		// Add a single proxied wildcard CNAME *.{effectiveDomain} so Cloudflare
		// Total TLS can issue a wildcard edge cert covering all app subdomains.
		// A wildcard record is preferred over per-app records because Total TLS
		// issues one wildcard cert rather than many individual certs.
		if r.CloudflareDNS != nil {
			wildcard := "*." + effectiveDomain
			if err := r.CloudflareDNS.ensureCNAME(ctx, wildcard, r.CloudflareDNS.tunnelCNAME); err != nil {
				// Non-fatal: DNS record creation failure should not block ingress
				// provisioning. Total TLS provisioning will retry on next reconcile.
				ctrl.LoggerFrom(ctx).Error(err, "ensure Cloudflare wildcard DNS CNAME", "host", wildcard)
			}
		}
	}

	// Build the set of expected ingress names for the current spec.apps.
	expectedIngresses := make(map[string]struct{}, len(ingressApps))
	for _, ia := range ingressApps {
		expectedIngresses[appIngressName(tenant.Name, ia.appProfile)] = struct{}{}
	}

	for _, ia := range ingressApps {
		host := ingressHost(ia.appProfile, ia.ingress, effectiveDomain)
		if err := r.ensureAppIngress(ctx, tenant, nsName, ia.appProfile, ia.ingress, host, tlsSecret); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Ingress for app %s: %w", ia.appProfile, err)
		}
	}

	// Delete any ingresses that were created by this operator for this tenant
	// but whose app is no longer in spec.apps (stale after an app removal).
	if err := r.deleteStaleIngressesForTenant(ctx, tenant, nsName, expectedIngresses); err != nil {
		return ctrl.Result{}, err
	}

	mode := "vanity"
	if !vanity {
		mode = "kernel-wildcard-fallback"
	}
	r.setCondition(tenant, conditionIngressReady, metav1.ConditionTrue,
		"Provisioned",
		fmt.Sprintf("Ingress provisioned for %d app(s) on %q (%s)", len(ingressApps), effectiveDomain, mode))
	return ctrl.Result{}, nil
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

// ensureKernelWildcardSecret replicates the kernel wildcard TLS Secret into
// the tenant namespace under a fixed name. Mirrors the
// ensureRegistryCredentials pattern. Soft-fails if the source Secret is not
// yet present (operator retries on the next reconcile).
func (r *TenantReconciler) ensureKernelWildcardSecret(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      kernelWildcardSourceSecret,
		Namespace: kernelWildcardSourceNamespace,
	}, source); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read source kernel wildcard secret %s/%s: %w",
			kernelWildcardSourceNamespace, kernelWildcardSourceSecret, err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kernelWildcardTenantSecret,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Type: source.Type,
		Data: source.Data,
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: kernelWildcardTenantSecret, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Data, desired.Data) || existing.Type != desired.Type {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Type = desired.Type
		existing.Data = desired.Data
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[tenantLabel] = tenant.Name
		existing.Labels[managedByLabel] = managedByValue
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// ensureTenantWildcardCertificate creates a wildcard cert-manager Certificate
// CR for *.<domain> (and <domain>) in the tenant namespace, using DNS-01 via
// the Cloudflare ClusterIssuer. Idempotent: existence is taken as up-to-date.
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
		desired := buildTenantWildcardCertificate(tenant, nsName, certName, secretName, domain)
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
) error {
	desired := buildAppIngress(tenant, nsName, appProfile, ingress, host, tlsSecret)
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

	// Tenant wildcard Certificate (vanity-domain mode). Safe to issue Delete
	// unconditionally; in fallback mode the resource does not exist and
	// IgnoreNotFound swallows the 404.
	wildcardCert := &unstructured.Unstructured{}
	wildcardCert.SetGroupVersionKind(certManagerCertGVK)
	wildcardCert.SetName(tenantWildcardCertName(tenant.Name))
	wildcardCert.SetNamespace(nsName)
	if err := r.Delete(ctx, wildcardCert); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete tenant wildcard Certificate: %w", err)
	}

	// Delete the Cloudflare wildcard CNAME *.{effectiveDomain} that was
	// created during provisioning.
	if tenant.HasVanityDomain() && r.CloudflareDNS != nil {
		wildcard := "*." + effectiveDomain
		if err := r.CloudflareDNS.deleteCNAME(ctx, wildcard); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "delete Cloudflare wildcard DNS CNAME", "host", wildcard)
		}
	}

	wildcardSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kernelWildcardTenantSecret,
			Namespace: nsName,
		},
	}
	if err := r.Delete(ctx, wildcardSecret); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete replicated kernel wildcard TLS secret: %w", err)
	}
	return nil
}

func buildTenantWildcardCertificate(
	tenant *gentianov1alpha1.Tenant,
	nsName, certName, secretName, domain string,
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
		"name": defaultDNS01ClusterIssuer,
		"kind": "ClusterIssuer",
	}, "spec", "issuerRef")
	return obj
}

func buildAppIngress(
	tenant *gentianov1alpha1.Tenant,
	nsName, appProfile string,
	ingress *gentianov1alpha1.IngressSpec,
	host, tlsSecret string,
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
		annotations[k] = v
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
