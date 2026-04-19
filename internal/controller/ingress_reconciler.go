// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"

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
	conditionIngressReady  = "IngressReady"
	certManagerGroup       = "cert-manager.io"
	certManagerVersion     = "v1"
	certManagerCertKind    = "Certificate"
	defaultClusterIssuer   = "letsencrypt-prod"
	defaultServicePort     = int32(80)
	defaultIngressClass    = "nginx"
)

var certManagerCertGVK = schema.GroupVersionKind{
	Group:   certManagerGroup,
	Version: certManagerVersion,
	Kind:    certManagerCertKind,
}

// ensureIngress creates or reconciles per-app Kubernetes Ingress resources and
// a per-tenant wildcard cert-manager Certificate CR.
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
			ingressApps = append(ingressApps, appIngress{appProfile: app.Profile, ingress: profile.Spec.Ingress})
		}
	}

	if len(ingressApps) == 0 {
		r.setCondition(tenant, conditionIngressReady, metav1.ConditionTrue,
			"NoIngressConfigured", "No apps require ingress provisioning")
		return ctrl.Result{}, nil
	}

	clusterIssuer := defaultClusterIssuer
	if ci := ingressApps[0].ingress.ClusterIssuer; ci != "" {
		clusterIssuer = ci
	}
	if err := r.ensureWildcardCertificate(ctx, tenant, nsName, clusterIssuer); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure wildcard Certificate: %w", err)
	}

	for _, ia := range ingressApps {
		if err := r.ensureAppIngress(ctx, tenant, nsName, ia.appProfile, ia.ingress); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure Ingress for app %s: %w", ia.appProfile, err)
		}
	}

	// TODO(inc10-dns): Provision DNS records via Tofu Controller Terraform CRs.

	r.setCondition(tenant, conditionIngressReady, metav1.ConditionTrue,
		"Provisioned", "Ingress resources and Certificate CR are provisioned")
	return ctrl.Result{}, nil
}

func (r *TenantReconciler) ensureWildcardCertificate(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	clusterIssuer string,
) error {
	desired := buildWildcardCertificate(tenant, nsName, clusterIssuer)
	name := wildcardCertName(tenant.Name)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(certManagerCertGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, existing); err != nil {
		if errors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	return nil
}

func (r *TenantReconciler) ensureAppIngress(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName, appProfile string,
	ingress *gentianov1alpha1.IngressSpec,
) error {
	desired := buildAppIngress(tenant, nsName, appProfile, ingress)
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
	}

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerCertGVK)
	cert.SetName(wildcardCertName(tenant.Name))
	cert.SetNamespace(nsName)
	if err := r.Delete(ctx, cert); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete wildcard Certificate CR: %w", err)
	}
	return nil
}

func buildWildcardCertificate(tenant *gentianov1alpha1.Tenant, nsName, clusterIssuer string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(certManagerCertGVK)
	obj.SetName(wildcardCertName(tenant.Name))
	obj.SetNamespace(nsName)
	obj.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})
	_ = unstructured.SetNestedStringSlice(obj.Object, []string{"*." + tenant.Spec.Domain, tenant.Spec.Domain}, "spec", "dnsNames")
	_ = unstructured.SetNestedField(obj.Object, wildcardCertSecretName(tenant.Name), "spec", "secretName")
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
) *networkingv1.Ingress {
	host := ingressHost(appProfile, ingress, tenant.Spec.Domain)
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
				SecretName: wildcardCertSecretName(tenant.Name),
			},
		}
	}
	return obj
}

func wildcardCertName(tenantName string) string {
	return fmt.Sprintf("wildcard-%s", tenantName)
}

func wildcardCertSecretName(tenantName string) string {
	return fmt.Sprintf("wildcard-%s-tls", tenantName)
}

func appIngressName(tenantName, appProfile string) string {
	return fmt.Sprintf("ingress-%s-%s", tenantName, appProfile)
}

func ingressHost(appProfile string, ingress *gentianov1alpha1.IngressSpec, tenantDomain string) string {
	sub := ingress.SubDomain
	if sub == "" {
		sub = appProfile
	}
	return fmt.Sprintf("%s.%s", sub, tenantDomain)
}
