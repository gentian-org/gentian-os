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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var helmReleaseGVK = schema.GroupVersionKind{
	Group:   "helm.crossplane.io",
	Version: "v1beta1",
	Kind:    "Release",
}

func kernelPortalHost(kernelDomain string) string {
	if kernelDomain == "" {
		return ""
	}
	return "portal." + kernelDomain
}

func kernelPortalURL(kernelDomain string) string {
	return "https://" + kernelPortalHost(kernelDomain) + "/login/"
}

// ensurePortalRedirect converges tenants onto the shared kernel portal login at
// portal.<kernel-domain>/login/. Legacy per-tenant portal stacks are removed;
// tenant effective domains redirect / to the shared login page.
func (r *TenantReconciler) ensurePortalRedirect(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if err := r.removeLegacyPerTenantPortalStack(ctx, tenant); err != nil {
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

// deletePortalRedirect removes any remaining per-tenant portal resources when the
// tenant CR is deleted with DeletionPolicy=Delete.
func (r *TenantReconciler) deletePortalRedirect(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	return r.removeLegacyPerTenantPortalStack(ctx, tenant)
}

func tenantPortalRedirectName(tenantName string) string {
	return fmt.Sprintf("tenant-%s-portal-redirect", tenantName)
}

func legacyPerTenantPortalResourceNames(tenantName string) []string {
	gentianLogin := fmt.Sprintf("umc-%s-gentian-login", tenantName)
	return []string{
		fmt.Sprintf("umc-%s-root-redirect", tenantName),
		fmt.Sprintf("umc-%s-login-redirect", tenantName),
		gentianLogin,
		gentianLogin + "-branding",
	}
}

func portalRedirectLabels(tenantName, instance string) map[string]string {
	return map[string]string{
		tenantLabel:                  tenantName,
		managedByLabel:               managedByValue,
		portalRedirectComponentLabel: portalRedirectComponentValue,
		"app.kubernetes.io/name":     "portal-redirect",
		"app.kubernetes.io/instance": instance,
	}
}

// removeLegacyPerTenantPortalStack deletes superseded per-tenant portal Helm releases,
// supporting secrets/configmaps, and legacy login ingresses. Idempotent.
func (r *TenantReconciler) removeLegacyPerTenantPortalStack(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	nsName := tenantNamespaceName(tenant)

	for _, releaseName := range []string{
		fmt.Sprintf("umc-%s", tenant.Name),
		fmt.Sprintf("umc-gateway-%s", tenant.Name),
	} {
		rel := &unstructured.Unstructured{}
		rel.SetGroupVersionKind(helmReleaseGVK)
		rel.SetName(releaseName)
		if err := r.Delete(ctx, rel); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Release %s: %w", releaseName, err)
		}
	}

	for _, name := range legacyPerTenantPortalResourceNames(tenant.Name) {
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
		"umc-ldap-admin",
		"umc-db-credentials",
		"umc-db-selfservice",
		"umc-smtp",
		"umc-oidc-client",
	} {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName}}
		if err := r.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "umc-ucr", Namespace: nsName}}
	if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
		return err
	}
	return nil
}

func (r *TenantReconciler) ensureTenantPortalRedirect(ctx context.Context, tenant *gentianov1alpha1.Tenant, effectiveDomain string) error {
	nsName := tenantNamespaceName(tenant)
	return r.ensureTenantPortalRedirectGateway(ctx, tenant, nsName, effectiveDomain)
}

func (r *TenantReconciler) ensureTenantPortalRedirectGateway(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName, effectiveDomain string) error {
	desired := buildTenantApexRedirectHTTPRoute(tenant, nsName, effectiveDomain, r.KernelDomain)
	return ensureHTTPRouteResource(ctx, r.Client, desired)
}
