/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/kernel/tenantshell"
	"github.com/gentian-org/gentian-os/internal/meta"
)

var xTenantListGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "XTenantList",
}

var appClaimListGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "AppList",
}

// startXTenantShellSimulator applies tenant shell resources when the operator
// creates an XTenant composite. Envtest has no Crossplane; this mimics
// tenant-default Composition behavior for controller integration tests.
func startXTenantShellSimulator(ctx context.Context, c client.Client) {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		cfg := netpolicy.DefaultConfig()
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

func simulateXTenantShellOnce(ctx context.Context, c client.Client, cfg netpolicy.Config, kubeAPIEndpts *discoveryv1.EndpointSlice) {
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
		if !xTenantShellActive(ctx, c, xr) {
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
		nsName := shellNamespaceName(tenant)
		createIfMissing(ctx, c, shellNamespace(tenantName, nsName))
		createIfMissing(ctx, c, shellLimitRange(tenantName, nsName))
		if rq := shellResourceQuota(tenantName, nsName, tenant.Spec.Quotas); rq != nil {
			createIfMissing(ctx, c, rq)
		}
		createIfMissing(ctx, c, netpolicy.BaselineNetworkPolicy(tenantName, nsName, cfg, kubeAPIEndpts))
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
		if !xTenantShellActive(ctx, c, xr) {
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
		tenantName := claim.GetLabels()[meta.TenantLabel]
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
		meta.ManagedByLabel: meta.ManagedByValue,
	}); err != nil {
		return
	}
	for i := range nsList.Items {
		ns := &nsList.Items[i]
		tenantName := ns.GetLabels()[meta.TenantLabel]
		if tenantName == "" {
			continue
		}
		if _, ok := activeTenants[tenantName]; ok {
			continue
		}
		nsName := ns.GetName()
		deleteSimulatedShellResources(ctx, c, nsName)
	}
}

// xTenantShellActive reports whether the envtest simulator should provision or
// retain tenant shell resources for an XTenant. While the Tenant CR is deleting
// (or gone) the operator owns teardown — recreating LimitRange/NetworkPolicy
// here races deleteOwnedResourcesInNamespace and flakes Retain deletion tests.
func xTenantShellActive(ctx context.Context, c client.Client, xr *unstructured.Unstructured) bool {
	if xr.GetDeletionTimestamp() != nil {
		return false
	}
	tenantCR := &gentianov1alpha1.Tenant{}
	err := c.Get(ctx, types.NamespacedName{Name: xr.GetName()}, tenantCR)
	if err != nil {
		return false
	}
	return tenantCR.GetDeletionTimestamp() == nil
}

func deleteSimulatedShellResources(ctx context.Context, c client.Client, nsName string) {
	for _, obj := range []client.Object{
		&corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "tenant-limits", Namespace: nsName}},
		&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "tenant-quota", Namespace: nsName}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "tenant-isolation", Namespace: nsName}},
	} {
		_ = client.IgnoreNotFound(c.Delete(ctx, obj))
	}
}

func tenantSpecFromXR(spec map[string]interface{}) gentianov1alpha1.TenantSpec {
	ts := gentianov1alpha1.TenantSpec{
		DisplayName: "Test Tenant"}
	if iso, ok := spec["isolation"].(map[string]interface{}); ok {
		if ns, ok := iso["namespace"].(string); ok && ns != "" {
			ts.Isolation = &gentianov1alpha1.TenantIsolation{Namespace: ns}
		}
	}
	if raw, ok := spec["quotas"].(map[string]interface{}); ok {
		q := &gentianov1alpha1.TenantQuotas{}
		// Every key xtenantQuotas emits is read back here. The simulator stands
		// in for the Composition, so a key it drops is a key the test suite
		// cannot see enforced — which is how requests.cpu and requests.memory
		// went missing from a real tenant while the suite stayed green.
		for key, target := range map[string]**resource.Quantity{
			"storage":        &q.Storage,
			"cpu":            &q.CPU,
			"memory":         &q.Memory,
			"requestsCpu":    &q.RequestsCPU,
			"requestsMemory": &q.RequestsMemory,
		} {
			if v, ok := raw[key].(string); ok && v != "" {
				parsed := resource.MustParse(v)
				*target = &parsed
			}
		}
		if v, ok := raw["maxApps"].(int64); ok {
			q.MaxApps = int32(v)
		}
		if v, ok := raw["maxPods"].(int64); ok {
			q.MaxPods = int32(v)
		}
		ts.Quotas = q
	}
	return ts
}

func createIfMissing(ctx context.Context, c client.Client, obj client.Object) {
	existing := obj.DeepCopyObject().(client.Object)
	err := c.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
	if errors.IsNotFound(err) {
		_ = c.Create(ctx, obj)
	}
}

// startNamespaceFinalizer stands in for the namespace controller, which
// envtest does not run: a namespace that has been deleted stays Terminating
// for ever here, because nothing removes the "kubernetes" finalizer from its
// spec. A tenant's deletion waits for its namespace to be gone, so the suite
// finishes the namespaces the way the real controller would once they are
// empty.
func startNamespaceFinalizer(ctx context.Context, c client.Client) {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				list := &corev1.NamespaceList{}
				if err := c.List(ctx, list); err != nil {
					continue
				}
				for i := range list.Items {
					ns := &list.Items[i]
					if ns.DeletionTimestamp == nil || len(ns.Spec.Finalizers) == 0 {
						continue
					}
					ns.Spec.Finalizers = nil
					_ = c.SubResource("finalize").Update(ctx, ns)
				}
			}
		}
	}()
}

// The tenant shell the tenant-default Composition composes, as the simulator
// creates it: the namespace, its LimitRange and its ResourceQuota.

func shellLabels(tenantName string) map[string]string {
	return map[string]string{
		meta.TenantLabel:    tenantName,
		meta.ManagedByLabel: meta.ManagedByValue,
	}
}

func shellNamespaceName(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.Namespace != "" {
		return tenant.Spec.Isolation.Namespace
	}
	return "tenant-" + tenant.Name
}

func shellNamespace(tenantName, nsName string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName, Labels: shellLabels(tenantName)}}
}

func shellLimitRange(tenantName, nsName string) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-limits", Namespace: nsName, Labels: shellLabels(tenantName)},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{{
				Type: corev1.LimitTypeContainer,
				Default: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Max: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			}},
		},
	}
}

func shellResourceQuota(tenantName, nsName string, quotas *gentianov1alpha1.TenantQuotas) *corev1.ResourceQuota {
	hard := tenantshell.ResourceListFromQuotas(quotas)
	if len(hard) == 0 {
		return nil
	}
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-quota", Namespace: nsName, Labels: shellLabels(tenantName)},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
	}
}
