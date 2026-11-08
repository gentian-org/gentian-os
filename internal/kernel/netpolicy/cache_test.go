/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func TestBuildDesired_TenantCachePolicies(t *testing.T) {
	t.Parallel()
	profile := &gentianov1alpha1.ComponentProfile{
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Cache: &gentianov1alpha1.CacheRequirement{
					Engine: gentianov1alpha1.CacheEngineMemcached,
				},
			}},
		},
	}
	in := netpolicy.BuildInput{
		TenantName: "demo",
		Namespace:  "tenant-demo",
		Apps:       []gentianov1alpha1.TenantApp{{Profile: "catalogue-test-app"}},
		Profiles:   map[string]*gentianov1alpha1.ComponentProfile{"catalogue-test-app": profile},
		Config:     netpolicy.DefaultConfig(),
	}
	policies := netpolicy.BuildDesired(in)
	var egress, ingress bool
	for _, np := range policies {
		switch np.Name {
		case "tenant-cache-egress":
			egress = true
			if got := np.Spec.PodSelector.MatchExpressions[0].Values[0]; got != "catalogue-test-app" {
				t.Fatalf("expected catalogue-test-app app selector, got %q", got)
			}
		case "tenant-cache-ingress":
			ingress = true
			if got := np.Spec.PodSelector.MatchLabels[meta.ComponentLabel]; got != meta.TenantCacheComponentValue {
				t.Fatalf("expected tenant-cache component selector, got %q", got)
			}
		}
	}
	if !egress || !ingress {
		t.Fatalf("expected tenant cache egress and ingress policies, got egress=%v ingress=%v", egress, ingress)
	}
	// Memcached's port and no other, in both directions.
	for _, np := range policies {
		var ports []networkingv1.NetworkPolicyPort
		switch np.Name {
		case "tenant-cache-egress":
			ports = np.Spec.Egress[0].Ports
		case "tenant-cache-ingress":
			ports = np.Spec.Ingress[0].Ports
		default:
			continue
		}
		if len(ports) != 1 || ports[0].Port.IntValue() != 11211 || *ports[0].Protocol != corev1.ProtocolTCP {
			t.Fatalf("%s: ports = %+v", np.Name, ports)
		}
	}
}

// A Redis app's cache is the shared Redis. It is not among the pods that may
// reach the tenant's Memcached, which holds other apps' cached data and asks
// for no password.
func TestARedisAppIsNotGivenTheTenantsMemcached(t *testing.T) {
	t.Parallel()
	cache := func(engine gentianov1alpha1.CacheEngine) *gentianov1alpha1.ComponentProfile {
		return &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Cache: &gentianov1alpha1.CacheRequirement{Engine: engine},
			}},
		}}
	}
	in := netpolicy.BuildInput{
		TenantName: "demo", Namespace: "tenant-demo",
		Apps: []gentianov1alpha1.TenantApp{{Profile: "with-redis"}, {Profile: "with-memcached"}, {Profile: "with-default"}},
		Profiles: map[string]*gentianov1alpha1.ComponentProfile{
			"with-redis":     cache(gentianov1alpha1.CacheEngineRedis),
			"with-memcached": cache(gentianov1alpha1.CacheEngineMemcached),
			"with-default":   cache(""),
		},
		Config: netpolicy.DefaultConfig(),
	}
	for _, np := range netpolicy.BuildDesired(in) {
		switch np.Name {
		case "tenant-cache-egress":
			if got := np.Spec.PodSelector.MatchExpressions[0].Values; len(got) != 1 || got[0] != "with-memcached" {
				t.Fatalf("pods that may reach memcached = %v", got)
			}
		case "tenant-cache-ingress":
			from := np.Spec.Ingress[0].From
			if len(from) != 1 || from[0].PodSelector.MatchLabels[meta.AppLabel] != "with-memcached" {
				t.Fatalf("pods memcached accepts = %+v", from)
			}
		}
	}
	in.Apps = in.Apps[:1]
	for _, np := range netpolicy.BuildDesired(in) {
		if np.Name == "tenant-cache-egress" || np.Name == "tenant-cache-ingress" {
			t.Fatalf("%s is built for a tenant with no memcached app", np.Name)
		}
	}
}
