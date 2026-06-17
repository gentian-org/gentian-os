/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the permissions and limitations under the License.
*/

package controller_test

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/tenantshell"
)

var xTenantListGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "XTenantList",
}

var appClaimGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "App",
}

var appClaimListGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "AppList",
}

// startXTenantShellSimulator applies tenant shell resources when the operator
// creates an XTenant composite. Envtest has no Crossplane; this mimics
// tenant-default Composition behaviour for controller integration tests.
func startXTenantShellSimulator(ctx context.Context, c client.Client) {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		cfg := tenantshell.DefaultConfig()
		var kubeAPIEndpts *discoveryv1.EndpointSlice
		endpts := &discoveryv1.EndpointSlice{}
		if err := c.Get(ctx, types.NamespacedName{Name: "kubernetes", Namespace: "default"}, endpts); err == nil {
			kubeAPIEndpts = endpts
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				simulateXTenantShellOnce(ctx, c, cfg, kubeAPIEndpts)
			}
		}
	}()
}

func simulateXTenantShellOnce(ctx context.Context, c client.Client, cfg tenantshell.Config, kubeAPIEndpts *discoveryv1.EndpointSlice) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(xTenantListGVK)
	if err := c.List(ctx, list); err != nil {
		return
	}
	for i := range list.Items {
		xr := &list.Items[i]
		if xr.GetDeletionTimestamp() != nil {
			continue
		}
		tenantName := xr.GetName()
		spec, _, _ := unstructured.NestedMap(xr.Object, "spec")
		if spec == nil {
			continue
		}
		tenant := &gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: tenantName},
			Spec:       tenantSpecFromXR(spec),
		}
		nsName := tenantshell.NamespaceName(tenant)
		createIfMissing(ctx, c, tenantshell.Namespace(tenantName, nsName))
		createIfMissing(ctx, c, tenantshell.LimitRange(tenantName, nsName))
		if rq := tenantshell.ResourceQuota(tenantName, nsName, tenant.Spec.Quotas); rq != nil {
			createIfMissing(ctx, c, rq)
		}
		createIfMissing(ctx, c, tenantshell.NetworkPolicy(tenantName, nsName, cfg, kubeAPIEndpts))
		simulateAppClaims(ctx, c, tenantName, nsName, spec)
		patchXTenantReady(ctx, c, xr)
	}
	simulateXTenantDeletionCascade(ctx, c, list.Items)
}

func patchXTenantReady(ctx context.Context, c client.Client, xr *unstructured.Unstructured) {
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(xr.GroupVersionKind())
	if err := c.Get(ctx, types.NamespacedName{Name: xr.GetName()}, current); err != nil {
		return
	}
	conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if typ, _ := cond["type"].(string); typ == "Ready" {
			if status, _ := cond["status"].(string); status == string(metav1.ConditionTrue) {
				return
			}
		}
	}
	now := metav1.Now()
	_ = unstructured.SetNestedSlice(current.Object, []interface{}{
		map[string]interface{}{
			"type":               "Ready",
			"status":             string(metav1.ConditionTrue),
			"reason":             "Available",
			"message":            "Simulated tenant shell Ready",
			"lastTransitionTime": now.Format(time.RFC3339),
		},
	}, "status", "conditions")
	_ = c.Status().Update(ctx, current)
}

func simulateXTenantDeletionCascade(ctx context.Context, c client.Client, activeXTenants []unstructured.Unstructured) {
	active := make(map[string]struct{}, len(activeXTenants))
	for i := range activeXTenants {
		xr := &activeXTenants[i]
		if xr.GetDeletionTimestamp() != nil {
			continue
		}
		active[xr.GetName()] = struct{}{}
	}

	claimList := &unstructured.UnstructuredList{}
	claimList.SetGroupVersionKind(appClaimListGVK)
	if err := c.List(ctx, claimList, client.MatchingLabels{
		"gentianos.io/managed-by": "crossplane",
	}); err != nil {
		return
	}
	for i := range claimList.Items {
		claim := &claimList.Items[i]
		tenantName := claim.GetLabels()[tenantshell.TenantLabel]
		if tenantName == "" {
			continue
		}
		if _, ok := active[tenantName]; !ok {
			_ = client.IgnoreNotFound(c.Delete(ctx, claim))
		}
	}

	simulateXTenantShellTeardown(ctx, c, active)
}

func simulateXTenantShellTeardown(ctx context.Context, c client.Client, activeTenants map[string]struct{}) {
	nsList := &corev1.NamespaceList{}
	if err := c.List(ctx, nsList, client.MatchingLabels{
		tenantshell.ManagedByLabel: tenantshell.ManagedByValue,
	}); err != nil {
		return
	}
	for i := range nsList.Items {
		ns := &nsList.Items[i]
		tenantName := ns.GetLabels()[tenantshell.TenantLabel]
		if tenantName == "" {
			continue
		}
		if _, ok := activeTenants[tenantName]; ok {
			continue
		}
		nsName := ns.GetName()
		for _, obj := range []client.Object{
			&corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "tenant-limits", Namespace: nsName}},
			&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "tenant-quota", Namespace: nsName}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "tenant-isolation", Namespace: nsName}},
		} {
			_ = client.IgnoreNotFound(c.Delete(ctx, obj))
		}
	}
}

func tenantSpecFromXR(spec map[string]interface{}) gentianov1alpha1.TenantSpec {
	ts := gentianov1alpha1.TenantSpec{}
	if iso, ok := spec["isolation"].(map[string]interface{}); ok {
		if ns, ok := iso["namespace"].(string); ok && ns != "" {
			ts.Isolation = &gentianov1alpha1.TenantIsolation{Namespace: ns}
		}
	}
	if raw, ok := spec["quotas"].(map[string]interface{}); ok {
		q := &gentianov1alpha1.TenantQuotas{}
		if v, ok := raw["storage"].(string); ok && v != "" {
			parsed := resource.MustParse(v)
			q.Storage = &parsed
		}
		if v, ok := raw["cpu"].(string); ok && v != "" {
			parsed := resource.MustParse(v)
			q.CPU = &parsed
		}
		if v, ok := raw["memory"].(string); ok && v != "" {
			parsed := resource.MustParse(v)
			q.Memory = &parsed
		}
		ts.Quotas = q
	}
	return ts
}

func simulateAppClaims(ctx context.Context, c client.Client, tenantName, nsName string, spec map[string]interface{}) {
	apps, _, _ := unstructured.NestedSlice(spec, "apps")
	desired := make(map[string]struct{}, len(apps))
	kd, _, _ := unstructured.NestedString(spec, "kernelDomain")
	domain, ok, _ := unstructured.NestedString(spec, "domain")
	if !ok || domain == "" {
		domain = tenantName + "." + kd
	}
	for _, raw := range apps {
		app, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		profile, _ := app["profile"].(string)
		if profile == "" {
			continue
		}
		desired[profile] = struct{}{}
		key := types.NamespacedName{Name: profile, Namespace: nsName}
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(appClaimGVK)
		err := c.Get(ctx, key, existing)
		if err == nil {
			continue
		}
		if !errors.IsNotFound(err) {
			continue
		}
		claim := &unstructured.Unstructured{}
		claim.SetGroupVersionKind(appClaimGVK)
		claim.SetName(profile)
		claim.SetNamespace(nsName)
		claim.SetLabels(map[string]string{
			tenantshell.TenantLabel:   tenantName,
			"gentianos.io/app":          profile,
			tenantshell.ManagedByLabel:  tenantshell.ManagedByValue,
			"gentianos.io/managed-by":   "crossplane",
		})
		_ = unstructured.SetNestedField(claim.Object, profile, "spec", "profileRef", "name")
		_ = unstructured.SetNestedField(claim.Object, nsName, "spec", "tenantNamespace")
		_ = unstructured.SetNestedField(claim.Object, domain, "spec", "domain")
		_ = unstructured.SetNestedField(claim.Object, "Automatic", "spec", "compositionUpdatePolicy")
		if variant, ok := app["variant"].(string); ok && variant != "" {
			_ = unstructured.SetNestedField(claim.Object, "app-"+variant, "spec", "compositionRef", "name")
		}
		_ = unstructured.SetNestedSlice(claim.Object, []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True", "reason": "Simulated", "message": "envtest"},
		}, "status", "conditions")
		_ = c.Create(ctx, claim)
	}

	claimList := &unstructured.UnstructuredList{}
	claimList.SetGroupVersionKind(appClaimListGVK)
	if err := c.List(ctx, claimList,
		client.InNamespace(nsName),
		client.MatchingLabels{
			tenantshell.TenantLabel:   tenantName,
			"gentianos.io/managed-by": "crossplane",
		},
	); err != nil {
		return
	}
	for i := range claimList.Items {
		claim := &claimList.Items[i]
		appName := claim.GetLabels()["gentianos.io/app"]
		if appName == "" {
			continue
		}
		if _, ok := desired[appName]; !ok {
			_ = client.IgnoreNotFound(c.Delete(ctx, claim))
		}
	}
}

func createIfMissing(ctx context.Context, c client.Client, obj client.Object) {
	existing := obj.DeepCopyObject().(client.Object)
	err := c.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
	if errors.IsNotFound(err) {
		_ = c.Create(ctx, obj)
	}
}
