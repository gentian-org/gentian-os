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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func issuerProfile(expose ...gentianov1alpha1.ExposureSpec) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{}
	p.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		Platform: &gentianov1alpha1.PlatformValueMapping{IssuerKey: "auth.issuer"},
	}
	p.Spec.Expose = expose
	return p
}

// A component told the issuer and handed no token by an exposure of its own
// is the one that needs the rule; one that is handed a token has the edge
// already, and one that is not told the issuer verifies nothing against it.
func TestWhoFetchesTheIssuersKeys(t *testing.T) {
	if !wantsIssuerKeys(issuerProfile()) {
		t.Error("a component told the issuer, with no exposure, is not given the keys")
	}
	if !wantsIssuerKeys(issuerProfile(gentianov1alpha1.ExposureSpec{Name: "web"})) {
		t.Error("an exposure that hands no token does not take the component to the edge")
	}
	if wantsIssuerKeys(issuerProfile(gentianov1alpha1.ExposureSpec{Name: "api", ExchangeToken: true})) {
		t.Error("a component handed an exchanged token has the edge already")
	}
	if wantsIssuerKeys(issuerProfile(gentianov1alpha1.ExposureSpec{Name: "api", ForwardToken: true})) {
		t.Error("a component handed a forwarded token has the edge already")
	}
	if wantsIssuerKeys(&gentianov1alpha1.ComponentProfile{}) {
		t.Error("a component not told the issuer is given the keys")
	}
}

// The rule is the edge Service's pods on the port its https port forwards
// to: not the http port, not the rest of the edge namespace, and the same
// with or without a LoadBalancer address.
func TestTheIssuersKeysAreTheEdgesHTTPSListenerAlone(t *testing.T) {
	for _, ip := range []string{"", "203.0.113.7"} {
		rule := issuerKeysEgress(edgeService(ip))
		if rule == nil {
			t.Fatalf("address %q: no rule", ip)
		}
		if len(rule.To) != 1 || rule.To[0].PodSelector == nil || rule.To[0].NamespaceSelector == nil {
			t.Fatalf("address %q: the rule names %v, want the Service's pods in its namespace", ip, rule.To)
		}
		if got := rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "kernel-edge" {
			t.Errorf("namespace = %q", got)
		}
		if got := rule.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"]; got != "envoy" {
			t.Errorf("the pods are selected by %v, want the Service's selector", rule.To[0].PodSelector.MatchLabels)
		}
		if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 10443 {
			t.Errorf("ports = %v, want 10443 alone", rule.Ports)
		}
	}
}

// A Service that selects nothing, or has no https port, gives no rule: a
// rule without a pod selector would be the whole namespace.
func TestNoRuleWithoutASelectorOrAnHTTPSPort(t *testing.T) {
	svc := edgeService("")
	svc.Spec.Selector = nil
	if issuerKeysEgress(svc) != nil {
		t.Error("a Service without a selector gave a rule")
	}
	svc = edgeService("")
	svc.Spec.Ports = svc.Spec.Ports[1:]
	if issuerKeysEgress(svc) != nil {
		t.Error("a Service without an https port gave a rule")
	}
	if issuerKeysEgress(nil) != nil {
		t.Error("no Service gave a rule")
	}
}

// Through the reconciler: the policy of a component told the issuer, with no
// exposure, is that one rule and no namespace-wide way to the edge.
func TestAComponentToldTheIssuerReachesTheEdgesListenerAndNothingElseThere(t *testing.T) {
	ctx := context.Background()
	scheme := appStoreScheme(t)
	tenant := acmeTenantFixture()
	profile := issuerProfile()
	profile.Name = "clerk"
	comp := tileFixtureComponent("clerk", "tenant-acme", "clerk")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp.DeepCopy(), edgeService("")).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelRealm: "kernel", KernelDomain: "k.example"}
	live := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(comp), live); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, live, profile, tenant, false); err != nil {
		t.Fatal(err)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "component-clerk", Namespace: "tenant-acme"}, np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("egress = %+v, want the one rule", np.Spec.Egress)
	}
	rule := &np.Spec.Egress[0]
	if len(rule.To) != 1 || rule.To[0].PodSelector == nil ||
		rule.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "envoy" {
		t.Fatalf("the rule names %+v, want the edge Service's pods", rule.To)
	}
	if got := ports(rule); len(got) != 1 || got[0] != 10443 {
		t.Fatalf("ports = %v, want 10443 alone", got)
	}
}
