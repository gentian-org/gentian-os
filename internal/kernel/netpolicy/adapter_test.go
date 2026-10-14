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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const instanceLabel = "app.kubernetes.io/instance"

func adapterProfile(adapts string, api bool) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
		Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassAdapter},
		Launch:  gentianov1alpha1.ComponentLaunchNone,
		Adapter: &gentianov1alpha1.AdapterSpec{
			SpeaksAs: gentianov1alpha1.AdapterSpeaksAsToken,
			Tools:    []gentianov1alpha1.ToolSpec{{Name: "t", Version: "v1", Description: map[string]string{"en": "t"}}},
		},
	}}
	if adapts != "" {
		p.Spec.Adapter.Adapts = &gentianov1alpha1.AdaptedApp{Profile: adapts}
	}
	if api {
		p.Spec.Package.API = &gentianov1alpha1.APIIntegration{BaseURL: "https://tools.example"}
	}
	return p
}

func appProfile() *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
		Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp},
		Launch:  gentianov1alpha1.ComponentLaunchNone,
	}}
}

func chaperoneProfile() *gentianov1alpha1.ComponentProfile {
	p := appProfile()
	p.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Vouching: &gentianov1alpha1.VouchingRequirement{},
	}}
	return p
}

func byName(policies []*networkingv1.NetworkPolicy) map[string]*networkingv1.NetworkPolicy {
	out := map[string]*networkingv1.NetworkPolicy{}
	for _, np := range policies {
		out[np.Name] = np
	}
	return out
}

func tenantWith(profiles map[string]*gentianov1alpha1.ComponentProfile, operatorInstalled ...string) netpolicy.BuildInput {
	in := netpolicy.BuildInput{
		TenantName:   "demo",
		Namespace:    "tenant-demo",
		Config:       netpolicy.DefaultConfig(),
		Profiles:     profiles,
		PodSelectors: map[string]map[string]string{},
	}
	for name := range profiles {
		in.Apps = append(in.Apps, gentianov1alpha1.TenantApp{Profile: name})
	}
	for _, name := range operatorInstalled {
		in.PodSelectors[name] = map[string]string{instanceLabel: "tenant-demo-" + name}
	}
	return in
}

// An adapter is reached by the component that speaks as a person and by
// nothing else: out of the baseline, which admits the edge, the identity
// provider and the operator to every pod, and into a floor of its own.
func TestAdapterIsReachedOnlyByTheComponentThatSpeaksAsAPerson(t *testing.T) {
	t.Parallel()
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{
		"openproject":         appProfile(),
		"openproject-adapter": adapterProfile("openproject", false),
		"chaperone":           chaperoneProfile(),
	}, "openproject-adapter")
	got := byName(netpolicy.BuildDesired(in))

	baseline := got["tenant-isolation"]
	want := []metav1.LabelSelectorRequirement{{
		Key: instanceLabel, Operator: metav1.LabelSelectorOpNotIn, Values: []string{"tenant-demo-openproject-adapter"},
	}}
	if len(baseline.Spec.PodSelector.MatchExpressions) != 1 ||
		baseline.Spec.PodSelector.MatchExpressions[0].Key != want[0].Key ||
		baseline.Spec.PodSelector.MatchExpressions[0].Operator != want[0].Operator ||
		baseline.Spec.PodSelector.MatchExpressions[0].Values[0] != want[0].Values[0] {
		t.Fatalf("the baseline must leave the adapter's pods out, got %+v", baseline.Spec.PodSelector)
	}

	floor := got["adapter-openproject-adapter"]
	if floor == nil {
		t.Fatal("the adapter has no policy of its own")
	}
	if floor.Labels[meta.NetPolicyTypeLabel] != meta.NetPolicyAdapter {
		t.Fatalf("labelled %q", floor.Labels[meta.NetPolicyTypeLabel])
	}
	if floor.Spec.PodSelector.MatchLabels[instanceLabel] != "tenant-demo-openproject-adapter" {
		t.Fatalf("selects %+v", floor.Spec.PodSelector)
	}
	if len(floor.Spec.Ingress) != 1 || len(floor.Spec.Ingress[0].From) != 1 ||
		floor.Spec.Ingress[0].From[0].PodSelector.MatchLabels[meta.AppLabel] != "chaperone" ||
		floor.Spec.Ingress[0].From[0].NamespaceSelector != nil {
		t.Fatalf("only the chaperone's pods may reach the adapter, got %+v", floor.Spec.Ingress)
	}
	// Its way out: the cluster's DNS, and the app it adapts.
	reachesApp := false
	for _, rule := range floor.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels[meta.AppLabel] == "openproject" {
				reachesApp = true
			}
		}
	}
	if !reachesApp {
		t.Fatalf("the adapter must reach the app it adapts, got %+v", floor.Spec.Egress)
	}

	target := got["adapter-target-openproject-adapter"]
	if target == nil || target.Spec.PodSelector.MatchLabels[meta.AppLabel] != "openproject" ||
		target.Spec.Ingress[0].From[0].PodSelector.MatchLabels[instanceLabel] != "tenant-demo-openproject-adapter" {
		t.Fatalf("the app must admit its adapter, got %+v", target)
	}

	callers := got["adapter-callers-chaperone"]
	if callers == nil || callers.Spec.PodSelector.MatchLabels[meta.AppLabel] != "chaperone" ||
		callers.Spec.Egress[0].To[0].PodSelector.MatchLabels[instanceLabel] != "tenant-demo-openproject-adapter" {
		t.Fatalf("the chaperone must reach the adapter, got %+v", callers)
	}

	names := netpolicy.ManagedPolicyNames(in)
	for _, name := range []string{"adapter-openproject-adapter", "adapter-target-openproject-adapter", "adapter-callers-chaperone"} {
		if _, ok := names[name]; !ok {
			t.Errorf("%s is built and not kept: it would be deleted on the next pass", name)
		}
	}
}

// With nothing installed that speaks as a person, nothing reaches an adapter.
func TestAdapterWithNobodyToCallItAdmitsNobody(t *testing.T) {
	t.Parallel()
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{
		"openproject-adapter": adapterProfile("openproject", false),
	})
	got := byName(netpolicy.BuildDesired(in))
	floor := got["adapter-openproject-adapter"]
	if floor == nil || len(floor.Spec.Ingress) != 0 {
		t.Fatalf("an adapter with no caller must admit nothing, got %+v", floor)
	}
	// The app it adapts is not installed: no way is opened to it.
	if _, ok := got["adapter-target-openproject-adapter"]; ok {
		t.Fatal("a way into an app that is not installed")
	}
	if len(floor.Spec.Egress) != len(got["tenant-isolation"].Spec.Egress) {
		t.Fatalf("an adapter whose app is absent reaches only what every pod does, got %+v", floor.Spec.Egress)
	}
}

// A tenant with no adapter keeps its baseline exactly as it was.
func TestNoAdapterLeavesTheBaselineAlone(t *testing.T) {
	t.Parallel()
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{
		"openproject": appProfile(),
		"chaperone":   chaperoneProfile(),
	})
	got := byName(netpolicy.BuildDesired(in))
	if exprs := got["tenant-isolation"].Spec.PodSelector.MatchExpressions; len(exprs) != 0 {
		t.Fatalf("no adapter, and the baseline leaves pods out: %+v", exprs)
	}
	for name := range got {
		if len(name) > 8 && name[:8] == "adapter-" {
			t.Errorf("%s built for a tenant with no adapter", name)
		}
	}
}

// An adapter that is an API runs nothing in the tenant. It is reached where it
// is, and the way out is the chaperone's: whatever egress a person granted the
// adapter, and nothing nobody granted.
func TestAnAPIAdapterIsReachedThroughTheChaperonesGrantedEgress(t *testing.T) {
	t.Parallel()
	tools := adapterProfile("", true)
	tools.Spec.Requires = &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
		Egress: []gentianov1alpha1.EgressRequest{{
			Name:   "tool-server",
			Rule:   networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.10/32"}}}},
			Reason: "the tool server this adapter offers",
		}},
	}}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"web-tools": tools,
		"chaperone": chaperoneProfile(),
	}

	in := tenantWith(profiles)
	got := byName(netpolicy.BuildDesired(in))
	if _, ok := got["adapter-web-tools"]; ok {
		t.Fatal("an API adapter runs no pods, and gets no floor")
	}
	if exprs := got["tenant-isolation"].Spec.PodSelector.MatchExpressions; len(exprs) != 0 {
		t.Fatalf("an API adapter has no pods to leave out: %+v", exprs)
	}
	if _, ok := got["adapter-callers-chaperone"]; ok {
		t.Fatal("egress nobody granted reached the chaperone")
	}

	in.Privileges = []gentianov1alpha1.TenantPrivilegeGrant{{
		Install: "web-tools", Privilege: "egress/tool-server", Approver: "perimeter-approver",
		ApprovedAt: metav1.Now(), Reason: "the team uses this search service",
	}}
	got = byName(netpolicy.BuildDesired(in))
	callers := got["adapter-callers-chaperone"]
	if callers == nil || len(callers.Spec.Egress) != 1 || callers.Spec.Egress[0].To[0].IPBlock.CIDR != "203.0.113.10/32" {
		t.Fatalf("the chaperone must reach the granted tool server, got %+v", callers)
	}
}
