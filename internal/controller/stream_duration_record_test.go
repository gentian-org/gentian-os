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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// streamDurationTenant is a tenant whose sessions last maxHours (0: the
// realm's default).
func streamDurationTenant(name string, maxHours int32) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if maxHours > 0 {
		t.Spec.Security = &gentianov1alpha1.TenantSecurity{Session: &gentianov1alpha1.SessionPolicy{MaxHours: maxHours}}
	}
	return t
}

// TestRecordStreamDurationPolicies records the timeouts of every
// BackendTrafficPolicy a signed-in stream passes through: the authenticated
// Gateway's, the identity provider's routes', and a sign-in sidecar's
// assertion route's, for clusters whose tenants' sessions last different
// lengths.
func TestRecordStreamDurationPolicies(t *testing.T) {
	cases := []struct {
		name    string
		tenants []*gentianov1alpha1.Tenant
	}{
		{"no tenants", nil},
		{"default sessions", []*gentianov1alpha1.Tenant{streamDurationTenant("demo", 0)}},
		{"8h and 24h sessions", []*gentianov1alpha1.Tenant{streamDurationTenant("short", 8), streamDurationTenant("long", 24), streamDurationTenant("demo", 0)}},
	}
	var b strings.Builder
	for _, tc := range cases {
		scheme := runtime.NewScheme()
		_ = gatewayv1.Install(scheme)
		_ = gentianov1alpha1.AddToScheme(scheme)
		objs := []client.Object{}
		for _, tenant := range tc.tenants {
			objs = append(objs, tenant)
		}
		comp := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-demo", UID: "uid-wiki"}}
		route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "wiki-sign-in", Namespace: "tenant-demo"}}
		objs = append(objs, comp, route)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		policies, err := recordStreamDurationPolicies(context.Background(), c, scheme, comp, route)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		fmt.Fprintf(&b, "%s:\n", tc.name)
		for _, p := range policies {
			timeout, _ := json.Marshal(p.spec["timeout"])
			fmt.Fprintf(&b, "  %s timeout=%s\n", p.name, timeout)
		}
	}
	holdToTranscript(t, *updateTranscripts, "stream-duration-policies", b.String())
}

type recordedPolicy struct {
	name string
	spec map[string]interface{}
}

// recordStreamDurationPolicies is each policy as the operator writes it.
func recordStreamDurationPolicies(ctx context.Context, c client.Client, scheme *runtime.Scheme, comp *gentianov1alpha1.Component, route *gatewayv1.HTTPRoute) ([]recordedPolicy, error) {
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", KernelRealm: "kernel", TenancyMode: "multi"}
	if err := r.ensureSignInACSRateLimit(ctx, comp, route); err != nil {
		return nil, err
	}
	sidecar := &unstructured.Unstructured{}
	sidecar.SetGroupVersionKind(backendTrafficPolicyGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, sidecar); err != nil {
		return nil, err
	}
	sidecarSpec, _, _ := unstructured.NestedMap(sidecar.Object, "spec")
	d, err := sessionStreamDuration(ctx, c)
	if err != nil {
		return nil, err
	}
	return []recordedPolicy{
		{"authenticated-gateway", authenticatedGatewayBackendTrafficPolicySpec(d)},
		{"identity-provider-proxy", withMaxStreamDuration(keycloakProxyBackendTrafficPolicySpec(), d)},
		{"identity-provider-realm", withMaxStreamDuration(keycloakRealmBackendTrafficPolicySpec(""), d)},
		{"sign-in-sidecar-assertion", sidecarSpec},
	}, nil
}

// The longest session is the bound, and a tenant's shorter one does not
// lower it for the rest.
func TestSessionStreamDurationIsTheLongestSession(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	for _, tc := range []struct {
		hours []int32
		want  string
	}{
		{nil, "12h"},
		{[]int32{0}, "12h"},
		{[]int32{1}, "12h"},
		{[]int32{1, 48}, "48h"},
		{[]int32{8760}, "8760h"},
	} {
		var objs []client.Object
		for i, h := range tc.hours {
			objs = append(objs, streamDurationTenant(fmt.Sprintf("t%d", i), h))
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		got, err := sessionStreamDuration(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("sessions %v: %s, want %s", tc.hours, got, tc.want)
		}
	}
}

// Setting the limit keeps the policy's other timeouts and leaves the policy
// it was given as it was.
func TestWithMaxStreamDurationKeepsTheRest(t *testing.T) {
	in := map[string]interface{}{"timeout": map[string]interface{}{"http": map[string]interface{}{"requestTimeout": "360s"}}}
	out := withMaxStreamDuration(in, "12h")
	got, _ := json.Marshal(out)
	if string(got) != `{"timeout":{"http":{"maxStreamDuration":"12h","requestTimeout":"360s"}}}` {
		t.Fatalf("out = %s", got)
	}
	was, _ := json.Marshal(in)
	if string(was) != `{"timeout":{"http":{"requestTimeout":"360s"}}}` {
		t.Fatalf("in changed: %s", was)
	}
}
