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

	// defaultHTTP01ClusterIssuer is used for per-host HTTP-01 certs issued
	// for tenants that have an explicit vanity domain. DNS-01 (and the
	// platform-wide Cloudflare token) is reserved for the kernel wildcard
	// and is never made available to tenant namespaces.
	// See docs/architecture.md §2.5.
	defaultHTTP01ClusterIssuer = "letsencrypt-http01"

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
//     "<sub>.<vanity_domain>". The operator creates one per-host
//     cert-manager Certificate CR per app, using an HTTP-01 ClusterIssuer.
//     The customer is responsible for the DNS records pointing at the
//     cluster ingress IP.
//
// IngressReady=True is reported once all required Ingress + Certificate
// resources have been applied. Cert-manager-side issuance status is
// tracked by the Certificate CRs themselves.
func (r *TenantReconciler) ensureIngress(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	nsName := tenantNamespaceName(tenant)

	type appIngress struct {
		appProfile    string
		ingress       *gentianov1alpha1.IngressSpec
		isolationMode gentianov1alpha1.AppDeploymentMode
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
				appProfile:    app.Profile,
				ingress:       profile.Spec.Ingress,
				isolationMode: app.IsolationMode,
			})
		}
	}

	if len(ingressApps) == 0 {
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

	// If any app is in shared mode, the Crossplane-managed ingresses in
	// shared-apps reference a secret named "wildcard-tls". Ensure it exists
	// there (copied from wildcard-kernel-tls in cert-manager) so TLS works
	// for matrix.* and other shared hostnames. This is idempotent and cheap.
	for _, ia := range ingressApps {
		if ia.isolationMode == gentianov1alpha1.AppDeploymentModeShared {
			if err := r.ensureSharedAppsWildcardTLS(ctx); err != nil {
				return ctrl.Result{}, fmt.Errorf("ensure wildcard-tls in shared-apps: %w", err)
			}
			break
		}
	}

	for _, ia := range ingressApps {
		// For shared apps the service lives in shared-apps, not in the tenant
		// namespace. nginx ingress resolves backends by namespace, so we
		// create an ExternalName service in the tenant namespace that
		// forwards to the real service in shared-apps.
		if ia.isolationMode == gentianov1alpha1.AppDeploymentModeShared {
			svcName := ia.ingress.ServiceName
			if svcName == "" {
				svcName = ia.appProfile
			}
			svcPort := ia.ingress.ServicePort
			if svcPort == 0 {
				svcPort = defaultServicePort
			}
			if err := r.ensureSharedAppProxySvc(ctx, tenant, nsName, svcName, svcPort); err != nil {
				return ctrl.Result{}, fmt.Errorf("ensure proxy service for shared app %s: %w", ia.appProfile, err)
			}
		}

		host := ingressHost(ia.appProfile, ia.ingress, effectiveDomain)
		tlsSecret := kernelWildcardTenantSecret
		if vanity {
			certName := perHostCertName(tenant.Name, ia.appProfile)
			tlsSecret = perHostCertSecretName(tenant.Name, ia.appProfile)
			issuer := ia.ingress.ClusterIssuer
			if issuer == "" {
				issuer = defaultHTTP01ClusterIssuer
			}
			if err := r.ensurePerHostCertificate(ctx, tenant, nsName, certName, tlsSecret, host, issuer); err != nil {
				return ctrl.Result{}, fmt.Errorf("ensure Certificate for app %s: %w", ia.appProfile, err)
			}
		}
		if err := r.ensureAppIngress(ctx, tenant, nsName, ia.appProfile, ia.ingress, host, tlsSecret); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Ingress for app %s: %w", ia.appProfile, err)
		}
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

// ensurePerHostCertificate creates a single-host cert-manager Certificate CR
// in the tenant namespace, using HTTP-01 via a ClusterIssuer. Idempotent:
// existence is taken as up-to-date; updates are not driven from the
// operator.
func (r *TenantReconciler) ensurePerHostCertificate(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName, certName, secretName, host, clusterIssuer string,
) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(certManagerCertGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: certName, Namespace: nsName}, existing); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		desired := buildPerHostCertificate(tenant, nsName, certName, secretName, host, clusterIssuer)
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

		// Per-host Certificate (vanity-domain mode). Safe to issue Delete
		// unconditionally; in fallback mode the resource simply does not
		// exist and IgnoreNotFound swallows the 404.
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certManagerCertGVK)
		cert.SetName(perHostCertName(tenant.Name, app.Profile))
		cert.SetNamespace(nsName)
		if err := r.Delete(ctx, cert); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Certificate for app %s: %w", app.Profile, err)
		}

		// ExternalName proxy service created for shared apps.
		if app.IsolationMode == gentianov1alpha1.AppDeploymentModeShared {
			svcName := profile.Spec.Ingress.ServiceName
			if svcName == "" {
				svcName = app.Profile
			}
			proxySvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svcName,
					Namespace: nsName,
				},
			}
			if err := r.Delete(ctx, proxySvc); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("delete proxy service for shared app %s: %w", app.Profile, err)
			}
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

func buildPerHostCertificate(
	tenant *gentianov1alpha1.Tenant,
	nsName, certName, secretName, host, clusterIssuer string,
) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(certManagerCertGVK)
	obj.SetName(certName)
	obj.SetNamespace(nsName)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	_ = unstructured.SetNestedStringSlice(obj.Object, []string{host}, "spec", "dnsNames")
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

// ensureSharedAppsWildcardTLS replicates wildcard-kernel-tls (cert-manager
// namespace) into shared-apps under the name "wildcard-tls". The Crossplane
// app compositions (opendesk-synapse-web et al.) hardcode secretName:
// wildcard-tls for their ingresses; without this secret nginx falls back to
// its self-signed default cert and browsers reject the connection.
//
// Called on every reconcile of a tenant that has at least one shared-mode app
// with ingress. Soft-fails if the source secret is not yet present (LE cert
// still pending) — the reconciler retries on the next cycle.
func (r *TenantReconciler) ensureSharedAppsWildcardTLS(ctx context.Context) error {
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      kernelWildcardSourceSecret,
		Namespace: kernelWildcardSourceNamespace,
	}, source); err != nil {
		if errors.IsNotFound(err) {
			return nil // cert not yet issued; will be retried on next reconcile
		}
		return fmt.Errorf("read kernel wildcard secret %s/%s: %w",
			kernelWildcardSourceNamespace, kernelWildcardSourceSecret, err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wildcard-tls",
			Namespace: sharedAppsNamespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
			},
		},
		Type: source.Type,
		Data: source.Data,
	}

	existing := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: "wildcard-tls", Namespace: sharedAppsNamespace}, existing); err != nil {
		if errors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Data, desired.Data) || existing.Type != desired.Type {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Type = desired.Type
		existing.Data = desired.Data
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// ensureSharedAppProxySvc creates or updates an ExternalName Service in the
// tenant namespace that forwards to the real service in shared-apps. This
// lets the per-tenant Ingress (which is scoped to the tenant namespace) route
// to a backend that lives in a different namespace — nginx ingress resolves
// service backends by namespace, so without this proxy the upstream is missing
// and returns 503.
func (r *TenantReconciler) ensureSharedAppProxySvc(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName, svcName string,
	svcPort int32,
) error {
	externalName := fmt.Sprintf("%s.%s.svc.cluster.local", svcName, sharedAppsNamespace)
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: externalName,
			Ports: []corev1.ServicePort{
				{Port: svcPort},
			},
		},
	}

	existing := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: nsName}, existing); err != nil {
		if errors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	if existing.Spec.ExternalName != externalName || existing.Spec.Type != corev1.ServiceTypeExternalName {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec.Type = corev1.ServiceTypeExternalName
		existing.Spec.ExternalName = externalName
		existing.Spec.Ports = desired.Spec.Ports
		existing.Spec.ClusterIP = ""
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func perHostCertName(tenantName, appProfile string) string {
	return fmt.Sprintf("app-%s-%s", tenantName, appProfile)
}

func perHostCertSecretName(tenantName, appProfile string) string {
	return fmt.Sprintf("app-%s-%s-tls", tenantName, appProfile)
}

func appIngressName(tenantName, appProfile string) string {
	return fmt.Sprintf("ingress-%s-%s", tenantName, appProfile)
}

func ingressHost(appProfile string, ingress *gentianov1alpha1.IngressSpec, effectiveDomain string) string {
	sub := ingress.SubDomain
	if sub == "" {
		sub = appProfile
	}
	return fmt.Sprintf("%s.%s", sub, effectiveDomain)
}
