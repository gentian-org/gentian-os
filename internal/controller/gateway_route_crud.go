/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	// Annotations too, not only the spec. A route's annotations are read:
	// the edge's authorization table is built from them, so a route whose
	// spec was right and whose annotations were an earlier build's kept
	// that build's answer for good -- a new annotation never reached a
	// route that already existed. Only the ones this asks for are set;
	// what something else put on the route stays.
	annotationsStale := false
	for k, v := range desired.Annotations {
		if existing.Annotations[k] != v {
			annotationsStale = true
			break
		}
	}
	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) && !annotationsStale {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec = desired.Spec
	if !equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		existing.Labels = desired.Labels
	}
	if annotationsStale {
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		for k, v := range desired.Annotations {
			existing.Annotations[k] = v
		}
	}
	return c.Patch(ctx, existing, patch)
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
	return r.deleteStaleClientTrafficPoliciesForTenant(ctx, tenant, nsName, nil)
}
