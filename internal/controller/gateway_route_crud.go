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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

var backendTrafficPolicyGVK = schema.GroupVersionKind{
	Group:   "gateway.envoyproxy.io",
	Version: "v1alpha1",
	Kind:    "BackendTrafficPolicy",
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

func (r *TenantReconciler) deleteStaleBackendTrafficPoliciesForTenant(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	expected map[string]struct{},
) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   backendTrafficPolicyGVK.Group,
		Version: backendTrafficPolicyGVK.Version,
		Kind:    backendTrafficPolicyGVK.Kind + "List",
	})
	if err := r.List(ctx, list,
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue, tenantLabel: tenant.Name, gatewayComponentLabel: gatewayComponentApp},
	); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list tenant BackendTrafficPolicies for stale cleanup: %w", err)
	}
	for i := range list.Items {
		name := list.Items[i].GetName()
		if expected != nil {
			if _, wanted := expected[name]; wanted {
				continue
			}
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale BackendTrafficPolicy %s: %w", name, err)
		}
	}
	return nil
}

func (r *TenantReconciler) deleteStaleClientTrafficPoliciesForTenant(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	expected map[string]struct{},
) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   clientTrafficPolicyGVK.Group,
		Version: clientTrafficPolicyGVK.Version,
		Kind:    clientTrafficPolicyGVK.Kind + "List",
	})
	if err := r.List(ctx, list,
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue, tenantLabel: tenant.Name, gatewayComponentLabel: gatewayComponentApp},
	); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list tenant ClientTrafficPolicies for stale cleanup: %w", err)
	}
	for i := range list.Items {
		name := list.Items[i].GetName()
		if expected != nil {
			if _, wanted := expected[name]; wanted {
				continue
			}
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale ClientTrafficPolicy %s: %w", name, err)
		}
	}
	return nil
}

func (r *TenantReconciler) deleteTenantHTTPRoutes(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	if err := r.deleteStaleHTTPRoutesForTenant(ctx, tenant, nsName, nil); err != nil {
		return err
	}
	if err := r.deleteStaleBackendTrafficPoliciesForTenant(ctx, tenant, nsName, nil); err != nil {
		return err
	}
	if err := r.deleteStaleClientTrafficPoliciesForTenant(ctx, tenant, nsName, nil); err != nil {
		return err
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
