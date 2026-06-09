/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var helmReleaseGVK = schema.GroupVersionKind{
	Group:   "helm.crossplane.io",
	Version: "v1beta1",
	Kind:    "Release",
}

const (
	umcLDAPSecretName          = "umc-ldap-admin"
	umcDBSecretName            = "umc-db-credentials"
	umcDBSelfServiceSecretName = "umc-db-selfservice"
	umcSMTPSecretName          = "umc-smtp"
	umcOIDCSecretName          = "umc-oidc-client"
	umcUCRConfigMapName        = "umc-ucr"
)

// ensureUMC converges tenants onto the shared kernel portal login at
// portal.<kernel-domain>/login/. Per-tenant UMC stacks are removed; tenant
// effective domains redirect / to the shared login page.
func (r *TenantReconciler) ensureUMC(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if err := r.removePerTenantUMCStack(ctx, tenant); err != nil {
		return err
	}
	if r.KernelDomain == "" {
		return nil
	}
	effectiveDomain := r.tenantEffectiveDomain(tenant)
	if effectiveDomain == "" || effectiveDomain == kernelPortalHost(r.KernelDomain) {
		return nil
	}
	return r.ensureTenantPortalRedirect(ctx, tenant, effectiveDomain)
}

// deleteUMC removes any remaining per-tenant UMC resources when the tenant CR
// is deleted with DeletionPolicy=Delete.
func (r *TenantReconciler) deleteUMC(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	return r.removePerTenantUMCStack(ctx, tenant)
}

func kernelPortalHost(kernelDomain string) string {
	return "portal." + kernelDomain
}

func kernelPortalURL(kernelDomain string) string {
	return fmt.Sprintf("https://%s/login/", kernelPortalHost(kernelDomain))
}

func umcReleaseName(tenantName string) string {
	return fmt.Sprintf("umc-%s", tenantName)
}

func umcGatewayReleaseName(tenantName string) string {
	return fmt.Sprintf("umc-gateway-%s", tenantName)
}

func tenantPortalRedirectName(tenantName string) string {
	return fmt.Sprintf("tenant-%s-portal-redirect", tenantName)
}

func legacyUMCResourceNames(tenantName string) []string {
	gentianLogin := fmt.Sprintf("umc-%s-gentian-login", tenantName)
	return []string{
		fmt.Sprintf("umc-%s-root-redirect", tenantName),
		fmt.Sprintf("umc-%s-login-redirect", tenantName),
		gentianLogin,
		gentianLogin + "-branding",
	}
}

func umcPortalRedirectLabels(tenantName, instance string) map[string]string {
	return map[string]string{
		tenantLabel:               tenantName,
		managedByLabel:            managedByValue,
		umcFrontendComponentLabel: umcFrontendComponentValue,
		"app.kubernetes.io/name":  "portal-redirect",
		"app.kubernetes.io/instance": instance,
	}
}

// removePerTenantUMCStack deletes superseded per-tenant UMC Helm releases,
// supporting secrets/configmaps, and legacy login ingresses. Idempotent.
func (r *TenantReconciler) removePerTenantUMCStack(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	nsName := tenantNamespaceName(tenant)

	for _, releaseName := range []string{umcReleaseName(tenant.Name), umcGatewayReleaseName(tenant.Name)} {
		rel := &unstructured.Unstructured{}
		rel.SetGroupVersionKind(helmReleaseGVK)
		rel.SetName(releaseName)
		if err := r.Delete(ctx, rel); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Release %s: %w", releaseName, err)
		}
	}

	for _, name := range legacyUMCResourceNames(tenant.Name) {
		for _, obj := range []client.Object{
			&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}},
		} {
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		deploy := &unstructured.Unstructured{}
		deploy.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
		deploy.SetName(name)
		deploy.SetNamespace(nsName)
		if err := r.Delete(ctx, deploy); client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	for _, name := range []string{
		umcLDAPSecretName,
		umcDBSecretName,
		umcDBSelfServiceSecretName,
		umcSMTPSecretName,
		umcOIDCSecretName,
	} {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}}
		if err := r.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: umcUCRConfigMapName, Namespace: nsName}}
	if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
		return err
	}
	return nil
}

func (r *TenantReconciler) ensureTenantPortalRedirect(ctx context.Context, tenant *gentianov1alpha1.Tenant, effectiveDomain string) error {
	nsName := tenantNamespaceName(tenant)
	name := tenantPortalRedirectName(tenant.Name)
	portalURL := kernelPortalURL(r.KernelDomain)
	ingressClass := "public"
	pathTypePrefix := networkingv1.PathTypePrefix
	labels := umcPortalRedirectLabels(tenant.Name, name)

	desired := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsName,
			Labels:    labels,
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/permanent-redirect":   portalURL,
				"nginx.ingress.kubernetes.io/ssl-redirect":       "false",
				"nginx.ingress.kubernetes.io/force-ssl-redirect": "false",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			TLS: []networkingv1.IngressTLS{{
				Hosts:      []string{effectiveDomain},
				SecretName: kernelWildcardTenantSecret,
			}},
			Rules: []networkingv1.IngressRule{{
				Host: effectiveDomain,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathTypePrefix,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "placeholder",
									Port: networkingv1.ServiceBackendPort{Name: "http"},
								},
							},
						}},
					},
				},
			}},
		},
	}

	// Redirect-only ingresses need a backend service for the ingress spec; use
	// any existing Service in the namespace (app releases always create one).
	svcName, err := r.firstServiceInNamespace(ctx, nsName)
	if err != nil {
		return err
	}
	if svcName == "" {
		// No apps yet — skip until a backend exists (redirect annotation still
		// requires a valid service reference in the ingress API).
		return nil
	}
	desired.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name = svcName

	existing := &networkingv1.Ingress{}
	err = r.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !ingressSpecEqual(existing, desired) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec = desired.Spec
		existing.Annotations = desired.Annotations
		existing.Labels = desired.Labels
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func (r *TenantReconciler) firstServiceInNamespace(ctx context.Context, nsName string) (string, error) {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list, client.InNamespace(nsName)); err != nil {
		return "", err
	}
	for i := range list.Items {
		name := list.Items[i].Name
		if strings.HasPrefix(name, "umc-") {
			continue
		}
		return name, nil
	}
	return "", nil
}

func ingressSpecEqual(a, b *networkingv1.Ingress) bool {
	return equality.Semantic.DeepEqual(a.Spec, b.Spec) &&
		equality.Semantic.DeepEqual(a.Annotations, b.Annotations) &&
		equality.Semantic.DeepEqual(a.Labels, b.Labels)
}
