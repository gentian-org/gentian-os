/*
Copyright 2026 Gentian Organization.

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

	"k8s.io/apimachinery/pkg/runtime/schema"

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

// ensurePortalRedirect converges tenants onto the shared Gentian portal login at
// portal.<kernel-domain>/login/. Tenant apex hostnames redirect / to that login page.
func (r *TenantReconciler) ensurePortalRedirect(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if r.KernelDomain == "" {
		return nil
	}
	effectiveDomain := r.tenantEffectiveDomain(tenant)
	if effectiveDomain == "" || effectiveDomain == kernelPortalHost(r.KernelDomain) {
		return nil
	}
	return r.ensureTenantPortalRedirect(ctx, tenant, effectiveDomain)
}

// deletePortalRedirect is a no-op hook kept for tenant deletion ordering.
func (r *TenantReconciler) deletePortalRedirect(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	_ = ctx
	_ = tenant
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

func tenantPortalRedirectName(tenantName string) string {
	return fmt.Sprintf("tenant-%s-portal-redirect", tenantName)
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
