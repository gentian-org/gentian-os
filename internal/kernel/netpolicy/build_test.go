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
	// Baseline plus the tenant-export policy: export pods exist in every
	// tenant namespace when a backup runs, whatever apps are installed.
	if len(policies) != 2 {
		t.Fatalf("expected baseline + tenant-export policies, got %d", len(policies))
	}
	if policies[0].Name != "tenant-isolation" {
		t.Fatalf("expected tenant-isolation, got %q", policies[0].Name)
	}
	if len(policies[0].Spec.Egress) < 1 {
		t.Fatal("expected DNS egress on baseline")
	}
}

func TestBuildDesired_KernelAndContractPolicies(t *testing.T) {
	t.Parallel()
	profile := &gentianov1alpha1.ComponentProfile{
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
				Database: &gentianov1alpha1.DatabaseRequirement{},
			}},
		},
	}
	binding := &gentianov1alpha1.IntegrationBinding{}
	binding.Name = "demo--consumer--file-store"
	binding.Namespace = "tenant-demo"
	binding.Spec = gentianov1alpha1.IntegrationBindingSpec{
		Contract:     "file-store",
		Consumer:     gentianov1alpha1.AppEndpoint{App: "consumer-app", Namespace: "tenant-demo"},
		Provider:     gentianov1alpha1.AppEndpoint{App: "provider-app", Namespace: "tenant-demo"},
		Capabilities: []string{"webdav:read"},
	}

	in := netpolicy.BuildInput{
		TenantName: "demo",
		Namespace:  "tenant-demo",
		Apps:       []gentianov1alpha1.TenantApp{{Profile: "consumer-app"}},
		Profiles:   map[string]*gentianov1alpha1.ComponentProfile{"consumer-app": profile},
		Bindings:   []*gentianov1alpha1.IntegrationBinding{binding},
		Config:     netpolicy.DefaultConfig(),
	}
	// Declared by the profiles and granted by nobody: no contract policy.
	if got := len(netpolicy.BuildDesired(in)); got != 4 {
		t.Fatalf("expected baseline + tenant-export + kernel + app-internal with no grant, got %d", got)
	}
	grant := &gentianov1alpha1.AppGrant{}
	grant.Spec.App = "consumer-app"
	grant.Spec.Consume = []gentianov1alpha1.ConsumeGrantSpec{{Contract: "file-store", Granted: []string{"webdav:read"}}}
	in.Grants = map[string]*gentianov1alpha1.AppGrant{"consumer-app": grant}
	policies := netpolicy.BuildDesired(in)
	if len(policies) != 6 {
		t.Fatalf("expected baseline + tenant-export + kernel + app-internal + the contract's two policies, got %d", len(policies))
	}
	contractNP := policies[len(policies)-1]
	if got := contractNP.Labels["gentianos.io/granted-capabilities"]; got != "webdav_read" {
		t.Fatalf("expected granted-capabilities label, got %q", got)
	}
}
