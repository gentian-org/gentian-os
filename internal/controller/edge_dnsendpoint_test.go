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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func dnsEndpointScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(dnsEndpointGVK)
	s.AddKnownTypeWithName(dnsEndpointGVK, u)
	list := &unstructured.UnstructuredList{}
	listGVK := dnsEndpointGVK
	listGVK.Kind += "List"
	list.SetGroupVersionKind(listGVK)
	s.AddKnownTypeWithName(listGVK, list)
	return s
}

// TestStaticIPClusterGetsNoEdgeDNSEndpoint is the invariant this change must
// not break: DNS on a static-ip cluster works today, published by external-dns
// from the Gateway's own address, and this path must write nothing there. A
// nil ingress IS the static-ip cluster, and so is one with no tunnel.
func TestStaticIPClusterGetsNoEdgeDNSEndpoint(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(dnsEndpointScheme(t)).Build()
	ctx := context.Background()

	if err := syncEdgeDNSEndpoint(ctx, c, nil, "platform-kernel",
		[]string{"id.example.test"}); err != nil {
		t.Fatalf("nil ingress: %v", err)
	}
	noTunnel := NewCloudflareTunnelIngress("t", "z", "", "")
	if err := syncEdgeDNSEndpoint(ctx, c, noTunnel, "platform-kernel",
		[]string{"id.example.test"}); err != nil {
		t.Fatalf("no tunnel: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(dnsEndpointGVK)
	err := c.Get(ctx, types.NamespacedName{Name: "edge-kernel", Namespace: "platform-kernel"}, got)
	if err == nil {
		t.Fatalf("a DNSEndpoint was written with no target to point at: %v", got.Object)
	}
}

// TestTunnelClusterPublishesEveryHostname: with a tunnel, every routed
// hostname becomes a CNAME to the tunnel, proxied, deduplicated and sorted —
// sorted because the object is re-reconciled every pass and a reordering set
// would rewrite it forever.
func TestTunnelClusterPublishesEveryHostname(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(dnsEndpointScheme(t)).Build()
	ctx := context.Background()
	ing := NewCloudflareTunnelIngress("t", "z", "abc-123.cfargotunnel.com", "")

	hosts := []string{"portal.example.test", "id.example.test", "portal.example.test", ""}
	if err := syncEdgeDNSEndpoint(ctx, c, ing, "platform-kernel", hosts); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(dnsEndpointGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-kernel", Namespace: "platform-kernel"}, got); err != nil {
		t.Fatalf("expected a DNSEndpoint: %v", err)
	}

	eps, _, _ := unstructured.NestedSlice(got.Object, "spec", "endpoints")
	if len(eps) != 2 {
		t.Fatalf("expected 2 deduplicated endpoints, got %d: %v", len(eps), eps)
	}
	first := eps[0].(map[string]interface{})
	if first["dnsName"] != "id.example.test" {
		t.Errorf("expected sorted order, got %v first", first["dnsName"])
	}
	if first["recordType"] != "CNAME" {
		t.Errorf("recordType = %v, want CNAME", first["recordType"])
	}
	targets, _ := first["targets"].([]interface{})
	if len(targets) != 1 || targets[0] != "abc-123.cfargotunnel.com" {
		t.Errorf("targets = %v, want the tunnel CNAME", targets)
	}
	if got.GetAnnotations()["external-dns.alpha.kubernetes.io/cloudflare-proxied"] != "true" {
		t.Error("a tunnel record must be proxied — cfargotunnel.com is unreachable unproxied")
	}
}

// TestEdgeDNSEndpointIsIdempotent: a second sync with the same hostnames
// updates in place rather than erroring on the existing object.
func TestEdgeDNSEndpointIsIdempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(dnsEndpointScheme(t)).Build()
	ctx := context.Background()
	ing := NewCloudflareTunnelIngress("t", "z", "abc-123.cfargotunnel.com", "")

	for i := 0; i < 2; i++ {
		if err := syncEdgeDNSEndpoint(ctx, c, ing, "platform-kernel",
			[]string{"id.example.test"}); err != nil {
			t.Fatalf("sync %d: %v", i+1, err)
		}
	}
}

// TestTenantEdgeDNSEndpointCarriesTheTenantLabel: teardown deletes by the
// tenant label pair, so an object without it would outlive its tenant and
// keep the tenant's records alive in the zone.
func TestTenantEdgeDNSEndpointCarriesTheTenantLabel(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(dnsEndpointScheme(t)).Build()
	ctx := context.Background()
	ing := NewCloudflareTunnelIngress("t", "z", "abc-123.cfargotunnel.com", "")

	if err := syncTenantEdgeDNSEndpoint(ctx, c, ing, "corp",
		[]string{"*.corp.example.test", "corp.example.test"}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(dnsEndpointGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "edge-corp", Namespace: defaultServicesNamespace()}, got); err != nil {
		t.Fatalf("expected the tenant DNSEndpoint: %v", err)
	}
	labels := got.GetLabels()
	if labels[tenantLabel] != "corp" || labels[managedByLabel] != managedByValue {
		t.Errorf("labels = %v; teardown selects on both, so both must be present", labels)
	}
}
