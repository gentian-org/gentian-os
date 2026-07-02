// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package netpolicy_test

import (
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

func TestEffectiveContractCapabilities(t *testing.T) {
	t.Parallel()
	binding := &gentianov1alpha1.IntegrationBinding{}
	binding.Spec = gentianov1alpha1.IntegrationBindingSpec{
		Contract:     "file-store",
		Capabilities: []string{"webdav:read", "webdav:write"},
	}

	t.Run("no grant uses binding", func(t *testing.T) {
		got := netpolicy.EffectiveContractCapabilities(binding, nil)
		if len(got) != 2 {
			t.Fatalf("expected 2 caps, got %v", got)
		}
	})

	t.Run("grant subset", func(t *testing.T) {
		grant := &gentianov1alpha1.AppGrant{}
		grant.Spec.Consume = []gentianov1alpha1.ConsumeGrantSpec{
			{Contract: "file-store", Granted: []string{"webdav:read"}},
		}
		got := netpolicy.EffectiveContractCapabilities(binding, grant)
		if len(got) != 1 || got[0] != "webdav:read" {
			t.Fatalf("expected [webdav:read], got %v", got)
		}
	})

	t.Run("empty grant denies", func(t *testing.T) {
		grant := &gentianov1alpha1.AppGrant{}
		grant.Spec.Consume = []gentianov1alpha1.ConsumeGrantSpec{
			{Contract: "file-store", Granted: nil},
		}
		got := netpolicy.EffectiveContractCapabilities(binding, grant)
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
}

func TestFormatCapabilityLabel(t *testing.T) {
	t.Parallel()
	got := netpolicy.FormatCapabilityLabel([]string{"webdav:read", "webdav:write"})
	if got != "webdav_read,webdav_write" {
		t.Fatalf("expected sanitized label, got %q", got)
	}
}

func TestBuildDesired_GrantIntersectionSkipsContractNP(t *testing.T) {
	t.Parallel()
	binding := &gentianov1alpha1.IntegrationBinding{}
	binding.Name = "demo--consumer--file-store"
	binding.Namespace = "tenant-demo"
	binding.Spec = gentianov1alpha1.IntegrationBindingSpec{
		Contract: "file-store",
		Consumer: gentianov1alpha1.AppEndpoint{App: "consumer-app"},
		Provider: gentianov1alpha1.AppEndpoint{App: "provider-app"},
		Capabilities: []string{"webdav:read"},
	}
	grant := &gentianov1alpha1.AppGrant{}
	grant.Spec.App = "consumer-app"
	grant.Spec.Consume = []gentianov1alpha1.ConsumeGrantSpec{
		{Contract: "file-store", Granted: []string{}},
	}

	in := netpolicy.BuildInput{
		TenantName: "demo",
		Namespace:  "tenant-demo",
		Bindings:   []*gentianov1alpha1.IntegrationBinding{binding},
		Grants:     map[string]*gentianov1alpha1.AppGrant{"consumer-app": grant},
		Config:     netpolicy.DefaultConfig(),
	}
	policies := netpolicy.BuildDesired(in)
	for _, np := range policies {
		if np.Name == "contract-demo--consumer--file-store" {
			t.Fatal("expected no contract NP when grant is empty")
		}
	}
}
