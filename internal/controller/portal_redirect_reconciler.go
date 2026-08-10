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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

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

func (r *TenantReconciler) ensurePortalRedirect(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	return r.deleteTenantPortalRedirect(ctx, tenant)
}

// deletePortalRedirect is the tenant-deletion hook; the route is removed on every
// reconcile, so deletion has nothing left to do.
func (r *TenantReconciler) deletePortalRedirect(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	return r.deleteTenantPortalRedirect(ctx, tenant)
}

func (r *TenantReconciler) deleteTenantPortalRedirect(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantApexRedirectRouteName(tenant.Name),
			Namespace: tenantNamespaceName(tenant),
		},
	}
	if err := r.Delete(ctx, route); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete legacy tenant portal redirect: %w", err)
	}
	return nil
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
