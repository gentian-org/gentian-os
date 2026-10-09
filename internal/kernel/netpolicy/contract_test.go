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

	networkingv1 "k8s.io/api/networking/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

func contractFixture() (*gentianov1alpha1.IntegrationBinding, *gentianov1alpha1.AppGrant) {
	binding := &gentianov1alpha1.IntegrationBinding{}
	binding.Name = "acme--engine--agent-tools"
	binding.Namespace = "tenant-acme"
	binding.Spec = gentianov1alpha1.IntegrationBindingSpec{
		Contract: "agent-tools",
		Consumer: gentianov1alpha1.AppEndpoint{App: "engine"},
		Provider: gentianov1alpha1.AppEndpoint{App: "chaperone"},
	}
	grant := &gentianov1alpha1.AppGrant{}
	grant.Spec.App = "engine"
	grant.Spec.Consume = []gentianov1alpha1.ConsumeGrantSpec{{Contract: "agent-tools", Granted: []string{"call"}}}
	return binding, grant
}

// A granted contract opens both sides, between the two apps and nobody else:
// a tenant's namespace is closed in both directions, so the consumer's way
// out alone reaches a provider that does not answer.
func TestAGrantedContractOpensBothSides(t *testing.T) {
	binding, grant := contractFixture()
	policies := netpolicy.ContractNetworkPolicies("acme", binding, grant, nil)
	if len(policies) != 2 {
		t.Fatalf("got %d policies, want the consumer's and the provider's", len(policies))
	}
	out, in := policies[0], policies[1]
	if out.Spec.PodSelector.MatchLabels[meta.AppLabel] != "engine" || len(out.Spec.PolicyTypes) != 1 ||
		out.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress ||
		out.Spec.Egress[0].To[0].PodSelector.MatchLabels[meta.AppLabel] != "chaperone" {
		t.Fatalf("the consumer's policy = %+v", out.Spec)
	}
	if in.Spec.PodSelector.MatchLabels[meta.AppLabel] != "chaperone" || len(in.Spec.PolicyTypes) != 1 ||
		in.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress ||
		len(in.Spec.Ingress) != 1 || len(in.Spec.Ingress[0].From) != 1 ||
		in.Spec.Ingress[0].From[0].PodSelector.MatchLabels[meta.AppLabel] != "engine" ||
		in.Spec.Ingress[0].From[0].NamespaceSelector != nil {
		t.Fatalf("the provider's policy = %+v", in.Spec)
	}
	if out.Name == in.Name {
		t.Fatal("the two policies share a name")
	}
}

// What two profiles declare is a request. Without the administrator's grant
// for this consumer and this contract nothing is opened, and a contract that
// lost its grant is no longer among the policies the tenant keeps.
func TestAContractNobodyGrantedOpensNothing(t *testing.T) {
	binding, grant := contractFixture()
	other := grant.DeepCopy()
	other.Spec.Consume[0].Contract = "calendar"
	empty := grant.DeepCopy()
	empty.Spec.Consume[0].Granted = nil
	for name, g := range map[string]*gentianov1alpha1.AppGrant{"no grant": nil, "another contract": other, "nothing granted": empty} {
		if got := netpolicy.ContractNetworkPolicies("acme", binding, g, nil); got != nil {
			t.Errorf("%s: %d policies", name, len(got))
		}
	}
	in := netpolicy.BuildInput{
		TenantName: "acme", Namespace: "tenant-acme", Config: netpolicy.DefaultConfig(),
		Bindings: []*gentianov1alpha1.IntegrationBinding{binding},
	}
	if _, kept := netpolicy.ManagedPolicyNames(in)["contract-acme--engine--agent-tools"]; kept {
		t.Fatal("an ungranted contract's policy is kept")
	}
	in.Grants = map[string]*gentianov1alpha1.AppGrant{"engine": grant}
	names := netpolicy.ManagedPolicyNames(in)
	built := 0
	for _, np := range netpolicy.BuildDesired(in) {
		if np.Labels["gentianos.io/granted-capabilities"] == "" {
			continue
		}
		built++
		if _, kept := names[np.Name]; !kept {
			t.Fatalf("policy %s is built and not kept", np.Name)
		}
	}
	if built != 2 {
		t.Fatalf("%d contract policies built, want 2", built)
	}
}

// Each side is selected by what its delivery puts on its pods.
func TestAContractSelectsEachSideByItsOwnLabels(t *testing.T) {
	binding, grant := contractFixture()
	selectors := map[string]map[string]string{"chaperone": {"app.kubernetes.io/instance": "tenant-acme-chaperone"}}
	policies := netpolicy.ContractNetworkPolicies("acme", binding, grant, selectors)
	out, in := policies[0], policies[1]
	if got := out.Spec.Egress[0].To[0].PodSelector.MatchLabels; got["app.kubernetes.io/instance"] != "tenant-acme-chaperone" || len(got) != 1 {
		t.Fatalf("the consumer reaches %v", got)
	}
	if got := in.Spec.PodSelector.MatchLabels; got["app.kubernetes.io/instance"] != "tenant-acme-chaperone" || len(got) != 1 {
		t.Fatalf("the provider's policy selects %v", got)
	}
	if got := in.Spec.Ingress[0].From[0].PodSelector.MatchLabels; got[meta.AppLabel] != "engine" {
		t.Fatalf("the provider admits %v", got)
	}
}

// An app does not open a way to itself through a contract: its own pods
// already reach each other.
func TestAContractWithItselfIsNoContract(t *testing.T) {
	binding, grant := contractFixture()
	binding.Spec.Provider.App = "engine"
	if got := netpolicy.ContractNetworkPolicies("acme", binding, grant, nil); got != nil {
		t.Fatalf("%d policies", len(got))
	}
}

// A component that vouches for people reaches the registrar on its one port,
// and the bouncer's listener for the key it is given as well.
func TestVouchingOpensTheRegistrarAndTheRightsCheck(t *testing.T) {
	t.Setenv("GENTIAN_NS_EDGE", "kernel-edge")
	t.Setenv("GENTIAN_NS_CONTROL", "kernel-control")
	cfg := netpolicy.DefaultConfig()
	cfg.ServicesNamespace = layout.Namespace(layout.Edge)
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
		Services: &gentianov1alpha1.ServiceRequirements{Vouching: &gentianov1alpha1.VouchingRequirement{}},
	}
	np := netpolicy.KernelAccessNetworkPolicy("acme", "tenant-acme", "notary", profile, cfg)
	if np == nil {
		t.Fatal("no policy for a component that vouches")
	}
	want := map[string]int32{cfg.ServicesNamespace: netpolicy.RightsCheckPort, layout.Namespace(layout.Control): netpolicy.RegistrarPort}
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.NamespaceSelector == nil {
				continue
			}
			ns := to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
			port, ok := want[ns]
			if !ok {
				continue
			}
			if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != port {
				t.Fatalf("%s is opened on %+v, want port %d alone", ns, rule.Ports, port)
			}
			delete(want, ns)
		}
	}
	if len(want) != 0 {
		t.Fatalf("no way to %v", want)
	}
	realm := false
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.NamespaceSelector != nil && to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == layout.Namespace(layout.Authentication) {
				realm = true
			}
		}
	}
	if !realm {
		t.Fatal("no way to the realm it posts its statements to")
	}
}

// An app that declares the rights check reaches the bouncer's listener for
// it and no other port of the edge.
func TestTheRightsCheckOpensOnePortOfTheEdge(t *testing.T) {
	t.Setenv("GENTIAN_NS_EDGE", "kernel-edge")
	cfg := netpolicy.DefaultConfig()
	cfg.ServicesNamespace = layout.Namespace(layout.Edge)
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
		Services: &gentianov1alpha1.ServiceRequirements{Rights: &gentianov1alpha1.RightsRequirement{}},
	}
	np := netpolicy.KernelAccessNetworkPolicy("acme", "tenant-acme", "notary", profile, cfg)
	if np == nil {
		t.Fatal("no policy for an app that declared the rights check")
	}
	found := false
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.NamespaceSelector == nil || to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != cfg.ServicesNamespace {
				continue
			}
			if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != netpolicy.RightsCheckPort {
				t.Fatalf("the edge is opened on %+v", rule.Ports)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no way to the edge in %+v", np.Spec.Egress)
	}
}
