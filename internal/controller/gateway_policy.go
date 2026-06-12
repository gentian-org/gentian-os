// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var backendTrafficPolicyGVK = schema.GroupVersionKind{
	Group:   "gateway.envoyproxy.io",
	Version: "v1alpha1",
	Kind:    "BackendTrafficPolicy",
}

func (r *TenantReconciler) collectTenantIngressIntents(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]ingressIntent, error) {
	var intents []ingressIntent
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.Ingress != nil {
			intents = append(intents, ingressIntent{appProfile: app.Profile, ingress: profile.Spec.Ingress})
		}
		for i := range profile.Spec.AdditionalIngresses {
			intents = append(intents, ingressIntent{
				appProfile: additionalIngressProfile(app.Profile, i),
				ingress:    &profile.Spec.AdditionalIngresses[i],
			})
		}
	}
	return intents, nil
}

func (r *TenantReconciler) ensureAppBackendTrafficPolicyWithRoute(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName, appProfile string,
	ingress *gentianov1alpha1.IngressSpec,
) error {
	spec := backendTrafficPolicySpecFromIngressAnnotations(ingress.Annotations)
	if spec == nil {
		return r.deleteBackendTrafficPolicy(ctx, nsName, appBackendTrafficPolicyName(tenant.Name, appProfile))
	}
	attachBackendTrafficPolicyTarget(spec, appHTTPRouteName(tenant.Name, appProfile))

	name := appBackendTrafficPolicyName(tenant.Name, appProfile)
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(backendTrafficPolicyGVK)
	desired.SetName(name)
	desired.SetNamespace(nsName)
	desired.SetLabels(map[string]string{
		tenantLabel:           tenant.Name,
		appLabel:              appProfile,
		managedByLabel:        managedByValue,
		gatewayComponentLabel: gatewayComponentApp,
	})
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(backendTrafficPolicyGVK)
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

func (r *TenantReconciler) deleteBackendTrafficPolicy(ctx context.Context, nsName, name string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(backendTrafficPolicyGVK)
	obj.SetName(name)
	obj.SetNamespace(nsName)
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

func backendTrafficPolicySpecFromIngressAnnotations(annotations map[string]string) map[string]interface{} {
	if len(annotations) == 0 {
		return nil
	}
	spec := map[string]interface{}{
		"targetRefs": []interface{}{
			map[string]interface{}{
				"group": "gateway.networking.k8s.io",
				"kind":  "HTTPRoute",
			},
		},
	}
	var timeout map[string]interface{}
	if d := nginxDurationAnnotation(annotations, "nginx.ingress.kubernetes.io/proxy-read-timeout"); d != "" {
		timeout = map[string]interface{}{"http": map[string]interface{}{"requestTimeout": d}}
	}
	if d := nginxDurationAnnotation(annotations, "nginx.ingress.kubernetes.io/proxy-send-timeout"); d != "" {
		if timeout == nil {
			timeout = map[string]interface{}{}
		}
		http, _ := timeout["http"].(map[string]interface{})
		if http == nil {
			http = map[string]interface{}{}
			timeout["http"] = http
		}
		http["responseTimeout"] = d
	}
	if timeout != nil {
		spec["timeout"] = timeout
	}
	if body := annotations[nginxProxyBodySizeAnnotation]; body != "" {
		spec["connection"] = map[string]interface{}{
			"bufferLimit": body,
		}
	}
	if len(spec) == 1 {
		return nil
	}
	return spec
}

func nginxDurationAnnotation(annotations map[string]string, key string) string {
	raw := strings.TrimSpace(annotations[key])
	if raw == "" {
		return ""
	}
	if strings.HasSuffix(raw, "s") || strings.HasSuffix(raw, "m") || strings.HasSuffix(raw, "h") {
		return raw
	}
	if sec, err := strconv.Atoi(raw); err == nil && sec > 0 {
		return fmt.Sprintf("%ds", sec)
	}
	return raw
}

func (r *TenantReconciler) ensureHTTPRouteResource(ctx context.Context, desired *gatewayv1.HTTPRoute) error {
	return ensureHTTPRouteResource(ctx, r.Client, desired)
}

func ensureHTTPRouteResource(ctx context.Context, c client.Client, desired *gatewayv1.HTTPRoute) error {
	existing := &gatewayv1.HTTPRoute{}
	err := c.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec = desired.Spec
		if !equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
			existing.Labels = desired.Labels
		}
		return c.Patch(ctx, existing, patch)
	}
	return nil
}

func (r *TenantReconciler) deleteStaleHTTPRoutesForTenant(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	expected map[string]struct{},
) error {
	list := &gatewayv1.HTTPRouteList{}
	if err := r.List(ctx, list,
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue, tenantLabel: tenant.Name, gatewayComponentLabel: gatewayComponentApp},
	); err != nil {
		return fmt.Errorf("list tenant HTTPRoutes for stale cleanup: %w", err)
	}
	for i := range list.Items {
		name := list.Items[i].Name
		if expected != nil {
			if _, wanted := expected[name]; wanted {
				continue
			}
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale HTTPRoute %s: %w", name, err)
		}
	}
	return nil
}

func (r *TenantReconciler) deleteTenantHTTPRoutes(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	if err := r.deleteStaleHTTPRoutesForTenant(ctx, tenant, nsName, nil); err != nil {
		return err
	}
	intents, err := r.collectTenantIngressIntents(ctx, tenant)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if err := r.deleteBackendTrafficPolicy(ctx, nsName, appBackendTrafficPolicyName(tenant.Name, intent.appProfile)); err != nil {
			return err
		}
	}
	apex := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: tenantApexRedirectRouteName(tenant.Name), Namespace: nsName}}
	return client.IgnoreNotFound(r.Delete(ctx, apex))
}

func httpRouteProgrammed(ctx context.Context, c client.Client, route *gatewayv1.HTTPRoute) (bool, string) {
	current := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, current); err != nil {
		return false, "HTTPRouteMissing"
	}
	for _, parent := range current.Status.Parents {
		for _, cond := range parent.Conditions {
			if cond.Type == string(gatewayv1.RouteConditionAccepted) && cond.Status == metav1.ConditionFalse {
				reason := cond.Reason
				if reason == "" {
					reason = "NotAccepted"
				}
				return false, reason
			}
		}
	}
	return true, "Accepted"
}

func attachBackendTrafficPolicyTarget(spec map[string]interface{}, routeName string) {
	refs, ok := spec["targetRefs"].([]interface{})
	if !ok || len(refs) == 0 {
		return
	}
	ref, ok := refs[0].(map[string]interface{})
	if !ok {
		return
	}
	ref["name"] = routeName
}
