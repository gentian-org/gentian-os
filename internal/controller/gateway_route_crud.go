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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	return ensureObject(ctx, c, desired, func(existing *gatewayv1.HTTPRoute) bool {
		// Annotations too, not only the spec. A route's annotations are
		// read: the edge's authorization table is built from them, so a
		// route whose spec was right and whose annotations were an earlier
		// build's kept that build's answer for good -- a new annotation
		// never reached a route that already existed. Only the ones this
		// asks for are set; what something else put on the route stays.
		annotationsStale := false
		for k, v := range desired.Annotations {
			if existing.Annotations[k] != v {
				annotationsStale = true
				break
			}
		}
		// Except the restore hold's, which goes when the desired route is
		// not held (restore_hold.go).
		_, heldNow := existing.Annotations[restoreHoldAnnotation]
		_, heldDesired := desired.Annotations[restoreHoldAnnotation]
		if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) && !annotationsStale && heldNow == heldDesired {
			return false
		}
		syncRestoreHoldAnnotation(existing, desired)
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
		return true
	})
}

// deleteTenantHTTPRoutes deletes every route and traffic policy the operator
// wrote for the tenant in its namespace. A traffic policy kind the cluster
// does not serve has none to delete.
func (r *TenantReconciler) deleteTenantHTTPRoutes(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	selected := []client.ListOption{
		client.InNamespace(nsName),
		client.MatchingLabels{managedByLabel: managedByValue, tenantLabel: tenant.Name, gatewayComponentLabel: gatewayComponentApp},
	}
	if err := deleteUnlisted(ctx, r.Client, "HTTPRoute", &gatewayv1.HTTPRouteList{}, nil, selected...); err != nil {
		return err
	}
	for _, gvk := range []schema.GroupVersionKind{backendTrafficPolicyGVK, clientTrafficPolicyGVK} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := deleteUnlisted(ctx, r.Client, gvk.Kind, list, nil, selected...); err != nil && !meta.IsNoMatchError(err) {
			return err
		}
	}
	return nil
}

// deleteUnlisted deletes every object opts list into list whose name keep
// does not hold; a nil keep holds none. kind names them in an error.
func deleteUnlisted(ctx context.Context, c client.Client, kind string, list client.ObjectList, keep map[string]struct{}, opts ...client.ListOption) error {
	if err := c.List(ctx, list, opts...); err != nil {
		return fmt.Errorf("list %s for stale cleanup: %w", kind, err)
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok {
			continue
		}
		if _, kept := keep[obj.GetName()]; kept {
			continue
		}
		if err := c.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale %s %s: %w", kind, obj.GetName(), err)
		}
	}
	return nil
}
