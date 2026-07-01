/*
Copyright 2026 The Gentian Authors.

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
)

func TestBuildDesired_BaselineOnly(t *testing.T) {
	t.Parallel()
	in := netpolicy.BuildInput{
		TenantName: "demo",
		Namespace:  "tenant-demo",
		Config:     netpolicy.DefaultConfig(),
	}
	policies := netpolicy.BuildDesired(in)
	if len(policies) != 2 {
		t.Fatalf("expected baseline + app-init policies, got %d", len(policies))
	}
	if policies[0].Name != "tenant-isolation" {
		t.Fatalf("expected tenant-isolation, got %q", policies[0].Name)
	}
	if policies[1].Name != "app-init-access" {
		t.Fatalf("expected app-init-access, got %q", policies[1].Name)
	}
	if len(policies[0].Spec.Egress) < 1 {
		t.Fatal("expected DNS egress on baseline")
	}
}

func TestBuildDesired_KernelAndContractPolicies(t *testing.T) {
	t.Parallel()
	profile := &gentianov1alpha1.AppProfile{
		Spec: gentianov1alpha1.AppProfileSpec{
			KernelRequirements: &gentianov1alpha1.KernelRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{},
			},
		},
	}
	binding := &gentianov1alpha1.IntegrationBinding{}
	binding.Name = "demo--consumer--file-store"
	binding.Namespace = "tenant-demo"
	binding.Spec = gentianov1alpha1.IntegrationBindingSpec{
		Contract: "file-store",
		Consumer: gentianov1alpha1.AppEndpoint{App: "consumer-app", Namespace: "tenant-demo"},
		Provider: gentianov1alpha1.AppEndpoint{App: "provider-app", Namespace: "tenant-demo"},
		Capabilities: []string{"webdav:read"},
	}

	in := netpolicy.BuildInput{
		TenantName: "demo",
		Namespace:  "tenant-demo",
		Apps:       []gentianov1alpha1.TenantApp{{Profile: "consumer-app"}},
		Profiles:   map[string]*gentianov1alpha1.AppProfile{"consumer-app": profile},
		Bindings:   []*gentianov1alpha1.IntegrationBinding{binding},
		Config:     netpolicy.DefaultConfig(),
	}
	policies := netpolicy.BuildDesired(in)
	if len(policies) != 5 {
		t.Fatalf("expected baseline + app-init + kernel + app-internal + contract policies, got %d", len(policies))
	}
	contractNP := policies[len(policies)-1]
	if got := contractNP.Labels["gentianos.io/granted-capabilities"]; got != "webdav:read" {
		t.Fatalf("expected granted-capabilities label, got %q", got)
	}
}
