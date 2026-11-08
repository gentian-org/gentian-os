/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package perimetertraffic

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const exposition = `# TYPE envoy_cluster_upstream_rq_total counter
envoy_cluster_upstream_rq_total{envoy_cluster_name="httproute/tenant-acme-dmz/perimeter-shop-web/rule/0"} %d
envoy_cluster_upstream_rq_total{envoy_cluster_name="httproute/tenant-acme-dmz/perimeter-shop-web/rule/1"} 4
envoy_cluster_upstream_rq_xx{envoy_response_code_class="2",envoy_cluster_name="httproute/tenant-acme-dmz/perimeter-shop-web/rule/0"} %d
envoy_cluster_upstream_rq_xx{envoy_response_code_class="4",envoy_cluster_name="httproute/tenant-acme-dmz/perimeter-shop-web/rule/0"} 2
envoy_cluster_upstream_rq_total{envoy_cluster_name="httproute/kernel-edge/desktop/rule/0"} 999
envoy_cluster_external_upstream_rq_xx{envoy_response_code_class="2",envoy_cluster_name="httproute/tenant-acme-dmz/perimeter-shop-web/rule/0"} 77
`

func expo(total, ok int) []byte {
	return []byte(fmt.Sprintf(exposition, total, ok))
}

func TestOnlyTheRulesOfTenantDMZsAreRead(t *testing.T) {
	got := Parse(expo(10, 8))
	if len(got) != 2 {
		t.Fatalf("rules = %v, want the two in tenant-acme-dmz", got)
	}
	first := got[Rule{Namespace: "tenant-acme-dmz", Route: "perimeter-shop-web", Index: 0}]
	if first != (Counts{Requests: 10, C2xx: 8, C4xx: 2}) {
		t.Fatalf("rule 0 = %+v", first)
	}
}

type memory struct{ rows map[string][]Row }

func (m *memory) Record(_ context.Context, rows []Row) error {
	m.rows["acme"] = append(m.rows["acme"], rows...)
	return nil
}
func (m *memory) Prune(context.Context, time.Time) error { return nil }

func TestTheDifferenceBetweenReadingsIsRecordedPerEntryAndPath(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	start := metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "envoy-1", Namespace: "kernel-edge", UID: types.UID("u1"),
			Labels: map[string]string{"app.kubernetes.io/managed-by": "envoy-gateway", "app.kubernetes.io/component": "proxy"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1", StartTime: &start},
	}
	prefix := gatewayv1.PathMatchPathPrefix
	shop, api := "/shop", "/api"
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: "perimeter-shop-web", Namespace: "tenant-acme-dmz",
			Labels: map[string]string{"app.kubernetes.io/name": "perimeter-proxy", componentLabel: "shop", exposureLabel: "web"},
		},
		Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{
			{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &prefix, Value: &shop}}}},
			{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &prefix, Value: &api}}}},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, route).Build()

	store := &memory{rows: map[string][]Row{}}
	old := storeFor
	storeFor = func(context.Context, client.Reader, string) (recorder, error) { return store, nil }
	defer func() { storeFor = old }()

	readings := [][]byte{expo(10, 8), expo(25, 20), expo(3, 3)}
	clock := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	s := &Sampler{
		Client: c,
		Scrape: func(context.Context, *corev1.Pod) ([]byte, error) {
			body := readings[0]
			readings = readings[1:]
			return body, nil
		},
		Now: func() time.Time { clock = clock.Add(15 * time.Minute); return clock },
	}

	s.RunOnce(context.Background()) // the baseline
	if len(store.rows["acme"]) != 0 {
		t.Fatalf("a baseline recorded traffic: %v", store.rows)
	}
	s.RunOnce(context.Background())
	rows := store.rows["acme"]
	if len(rows) != 1 || rows[0].Path != "/shop" || rows[0].Component != "shop" || rows[0].Exposure != "web" {
		t.Fatalf("rows = %+v, want /shop of shop/web (the /api rule did not move)", rows)
	}
	if rows[0].Counts != (Counts{Requests: 15, C2xx: 12}) {
		t.Fatalf("difference = %+v, want 15 requests, 12 of them 2xx", rows[0].Counts)
	}

	// Envoy restarted: its counters started again, and all it reads is new.
	store.rows["acme"] = nil
	s.RunOnce(context.Background())
	if got := store.rows["acme"]; len(got) != 1 || got[0].Requests != 3 {
		t.Fatalf("after a reset = %+v, want the 3 it counted since", got)
	}
}

func TestATenantIsReadOnlyFromItsOwnDMZ(t *testing.T) {
	for ns, want := range map[string]string{"tenant-acme-dmz": "acme", "tenant-a-b-dmz": "a-b"} {
		if got, ok := tenantOfDMZ(ns); !ok || got != want {
			t.Errorf("%s = %q %v", ns, got, ok)
		}
	}
	for _, ns := range []string{"tenant-acme", "kernel-edge", "tenant--dmz"} {
		if _, ok := tenantOfDMZ(ns); ok {
			t.Errorf("%s read as a DMZ", ns)
		}
	}
}
