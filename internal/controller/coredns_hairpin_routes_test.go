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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func hairpinTestRoute(ns, name string, parent gatewayv1.ParentReference, hosts ...string) *gatewayv1.HTTPRoute {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	route.Spec.ParentRefs = []gatewayv1.ParentReference{parent}
	for _, h := range hosts {
		route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(h))
	}
	return route
}

func edgeTestParentRef(gateway string) gatewayv1.ParentReference {
	ref := gatewayParentRef(gateway)
	ns := gatewayv1.Namespace(servicesNamespace)
	ref.Namespace = &ns
	return ref
}

// The consoles a tenant's install routes are on the edge but not among the
// hosts the operator derives: the hairpin takes them from the routes.
func TestCollectEdgeRouteHairpinHosts(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gatewayv1.Install(scheme)
	otherNS := gatewayv1.Namespace("elsewhere")
	objs := []client.Object{
		hairpinTestRoute("tenant-platform", "admin-console-web",
			edgeTestParentRef(AuthenticatedGatewayName), "admin.platform.example.org"),
		hairpinTestRoute("tenant-acme-dmz", "share",
			edgeTestParentRef(PerimeterGatewayName), "share.acme.example"),
		hairpinTestRoute("tenant-acme", "wildcard",
			edgeTestParentRef(AuthenticatedGatewayName), "*.acme.example.org"),
		hairpinTestRoute("tenant-acme", "own-gateway",
			gatewayv1.ParentReference{Name: AuthenticatedGatewayName, Namespace: &otherNS}, "own.example.org"),
		hairpinTestRoute("tenant-acme", "redirect", edgeTestParentRef(PerimeterGatewayName)),
		// Unqualified: the Gateway of the route's own namespace.
		hairpinTestRoute("tenant-acme", "local", gatewayParentRef(AuthenticatedGatewayName), "local.example.org"),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	got, err := collectEdgeRouteHairpinHosts(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{}{
		"admin.platform.example.org": {},
		"share.acme.example":         {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
}
