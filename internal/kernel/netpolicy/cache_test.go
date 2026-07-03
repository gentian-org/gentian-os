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

package netpolicy_test

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func TestBuildDesired_TenantCachePolicies(t *testing.T) {
	t.Parallel()
	profile := &gentianov1alpha1.AppProfile{
		Spec: gentianov1alpha1.AppProfileSpec{
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Cache: &gentianov1alpha1.CacheRequirement{
					Engine: gentianov1alpha1.CacheEngineMemcached,
				},
			},
		},
	}
	in := netpolicy.BuildInput{
		TenantName: "demo",
		Namespace:  "tenant-demo",
		Apps:       []gentianov1alpha1.TenantApp{{Profile: "catalogue-test-app"}},
		Profiles:   map[string]*gentianov1alpha1.AppProfile{"catalogue-test-app": profile},
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
}
