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
	"errors"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A kernel route that is withdrawn -- the model gateway's console on a
// cluster whose claim does not switch it on -- goes before the policy that
// put it behind the session, and the policy stays if the route could not be
// removed. The other order leaves a route with a backend and no question.
func TestAWithdrawnKernelRouteGoesBeforeItsPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{managedByLabel: managedByValue, gatewayComponentLabel: gatewayComponentKernel}
	fixtures := func() []client.Object {
		policy := &unstructured.Unstructured{}
		policy.SetGroupVersionKind(securityPolicyGVK)
		policy.SetName(kernelSecurityPolicyName(kernelRouteLiteLLM))
		policy.SetNamespace(servicesNamespace)
		policy.SetLabels(labels)
		return []client.Object{
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: kernelRouteLiteLLM, Namespace: servicesNamespace, Labels: labels}},
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: kernelRouteArgoCD, Namespace: servicesNamespace, Labels: labels}},
			policy,
		}
	}
	kept := map[string]struct{}{kernelRouteArgoCD: {}}
	policyLeft := func(c client.Client) bool {
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(securityPolicyGVK)
		err := c.Get(context.Background(), types.NamespacedName{Name: kernelSecurityPolicyName(kernelRouteLiteLLM), Namespace: servicesNamespace}, got)
		return err == nil
	}

	var order []string
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fixtures()...).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			kind := "SecurityPolicy"
			if _, isRoute := obj.(*gatewayv1.HTTPRoute); isRoute {
				kind = "HTTPRoute"
			}
			order = append(order, kind+"/"+obj.GetName())
			return w.Delete(ctx, obj, opts...)
		},
	}).Build()
	r := &GatewayPlatformReconciler{Client: c}
	if err := r.deleteStaleKernelRoutesAndPolicies(context.Background(), kept, map[string]struct{}{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"HTTPRoute/" + kernelRouteLiteLLM, "SecurityPolicy/" + kernelSecurityPolicyName(kernelRouteLiteLLM)}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("deleted in the order %v, want %v", order, want)
	}
	left := &gatewayv1.HTTPRouteList{}
	if err := c.List(context.Background(), left); err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 1 || left.Items[0].Name != kernelRouteArgoCD {
		t.Fatalf("routes left: %v", left.Items)
	}

	// The route cannot be removed: its policy is not touched.
	broken := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fixtures()...).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, isRoute := obj.(*gatewayv1.HTTPRoute); isRoute {
				return errors.New("the API server refused")
			}
			return w.Delete(ctx, obj, opts...)
		},
	}).Build()
	r = &GatewayPlatformReconciler{Client: broken}
	if err := r.deleteStaleKernelRoutesAndPolicies(context.Background(), kept, map[string]struct{}{}); err == nil {
		t.Fatal("a route that could not be removed was reported as removed")
	}
	if !policyLeft(broken) {
		t.Fatal("the route is still there and its policy is gone: it would answer without a session")
	}
}
