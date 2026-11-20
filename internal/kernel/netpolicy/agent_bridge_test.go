/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy_test

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const instanceLabel = "app.kubernetes.io/instance"

func agentBridgeProfile(connects string, api bool) *gentianov1alpha1.ComponentProfile {
	way := gentianov1alpha1.BridgeWay{Name: "tools", Caller: gentianov1alpha1.BridgeCallerAgents,
		Callee: gentianov1alpha1.BridgeCalleeApp, Protocol: gentianov1alpha1.BridgeProtocolMCP,
		SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson}
	p := &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
		Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassBridge},
		Launch:  gentianov1alpha1.ComponentLaunchNone,
		Bridge:  &gentianov1alpha1.BridgeSpec{},
	}}
	if connects != "" {
		p.Spec.Bridge.App = &gentianov1alpha1.BridgedApp{Profile: connects}
	} else {
		way.Callee = gentianov1alpha1.BridgeCalleeOutside
	}
	p.Spec.Bridge.Ways = []gentianov1alpha1.BridgeWay{way}
	if api {
		p.Spec.Package.API = &gentianov1alpha1.APIIntegration{BaseURL: "https://tools.example"}
	}
	return p
}

// syncBridgeProfile is a bridge with no way from the agents.
func syncBridgeProfile(connects string) *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
		Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassBridge},
		Launch:  gentianov1alpha1.ComponentLaunchNone,
		Bridge: &gentianov1alpha1.BridgeSpec{App: &gentianov1alpha1.BridgedApp{Profile: connects},
			Ways: []gentianov1alpha1.BridgeWay{{Name: "files", Caller: gentianov1alpha1.BridgeCallerDevice,
				Callee: gentianov1alpha1.BridgeCalleeApp, Protocol: gentianov1alpha1.BridgeProtocolWebDAV,
				SpeaksAs: gentianov1alpha1.BridgeSpeakerPerson, Paths: []string{"/dav"}}}},
	}}
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

// An agent bridge is reached by the component that speaks as a person and by
// nothing else: out of the baseline, which admits the edge, the identity
// provider and the operator to every pod, and into a floor of its own.
func TestAgentBridgeIsReachedOnlyByTheComponentThatSpeaksAsAPerson(t *testing.T) {
	t.Parallel()
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{
		"openproject":         appProfile(),
		"openproject-adapter": agentBridgeProfile("openproject", false),
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
		t.Fatalf("the baseline must leave the bridge's pods out, got %+v", baseline.Spec.PodSelector)
	}

	floor := got["agent-bridge-openproject-adapter"]
	if floor == nil {
		t.Fatal("the bridge has no policy of its own")
	}
	if floor.Labels[meta.NetPolicyTypeLabel] != meta.NetPolicyAgentBridge {
		t.Fatalf("labeled %q", floor.Labels[meta.NetPolicyTypeLabel])
	}
	if floor.Spec.PodSelector.MatchLabels[instanceLabel] != "tenant-demo-openproject-adapter" {
		t.Fatalf("selects %+v", floor.Spec.PodSelector)
	}
	if len(floor.Spec.Ingress) != 1 || len(floor.Spec.Ingress[0].From) != 1 ||
		floor.Spec.Ingress[0].From[0].PodSelector.MatchLabels[meta.AppLabel] != "chaperone" ||
		floor.Spec.Ingress[0].From[0].NamespaceSelector != nil {
		t.Fatalf("only the chaperone's pods may reach the bridge, got %+v", floor.Spec.Ingress)
	}
	// Its way out: the cluster's DNS, and the app it connects.
	reachesApp := false
	for _, rule := range floor.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels[meta.AppLabel] == "openproject" {
				reachesApp = true
			}
		}
	}
	if !reachesApp {
		t.Fatalf("the bridge must reach the app it connects, got %+v", floor.Spec.Egress)
	}

	target := got["agent-bridge-target-openproject-adapter"]
	if target == nil || target.Spec.PodSelector.MatchLabels[meta.AppLabel] != "openproject" ||
		target.Spec.Ingress[0].From[0].PodSelector.MatchLabels[instanceLabel] != "tenant-demo-openproject-adapter" {
		t.Fatalf("the app must admit its bridge, got %+v", target)
	}

	callers := got["agent-bridge-callers-chaperone"]
	if callers == nil || callers.Spec.PodSelector.MatchLabels[meta.AppLabel] != "chaperone" ||
		callers.Spec.Egress[0].To[0].PodSelector.MatchLabels[instanceLabel] != "tenant-demo-openproject-adapter" {
		t.Fatalf("the chaperone must reach the bridge, got %+v", callers)
	}

	names := netpolicy.ManagedPolicyNames(in)
	for _, name := range []string{"agent-bridge-openproject-adapter", "agent-bridge-target-openproject-adapter", "agent-bridge-callers-chaperone"} {
		if _, ok := names[name]; !ok {
			t.Errorf("%s is built and not kept: it would be deleted on the next pass", name)
		}
	}
}

// With nothing installed that speaks as a person, nothing reaches an agent bridge.
func TestAgentBridgeWithNobodyToCallItAdmitsNobody(t *testing.T) {
	t.Parallel()
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{
		"openproject-adapter": agentBridgeProfile("openproject", false),
	})
	got := byName(netpolicy.BuildDesired(in))
	floor := got["agent-bridge-openproject-adapter"]
	if floor == nil || len(floor.Spec.Ingress) != 0 {
		t.Fatalf("an agent bridge with no caller must admit nothing, got %+v", floor)
	}
	// The app it connects is not installed: no way is opened to it.
	if _, ok := got["agent-bridge-target-openproject-adapter"]; ok {
		t.Fatal("a way into an app that is not installed")
	}
	if len(floor.Spec.Egress) != len(got["tenant-isolation"].Spec.Egress) {
		t.Fatalf("an agent bridge whose app is absent reaches only what every pod does, got %+v", floor.Spec.Egress)
	}
}

// A tenant with no agent bridge keeps its baseline exactly as it was; a sync
// bridge, which has no way from the agents, changes nothing of it.
func TestNoAgentBridgeLeavesTheBaselineAlone(t *testing.T) {
	t.Parallel()
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{
		"openproject": appProfile(),
		"chaperone":   chaperoneProfile(),
		"sync":        syncBridgeProfile("openproject"),
	})
	got := byName(netpolicy.BuildDesired(in))
	if exprs := got["tenant-isolation"].Spec.PodSelector.MatchExpressions; len(exprs) != 0 {
		t.Fatalf("no agent bridge, and the baseline leaves pods out: %+v", exprs)
	}
	for name := range got {
		if strings.HasPrefix(name, "agent-bridge-") {
			t.Errorf("%s built for a tenant with no agent bridge", name)
		}
	}
}

// An agent bridge that is an API runs nothing in the tenant. It is reached where it
// is, and the way out is the chaperone's: whatever egress a person granted the
// bridge, and nothing nobody granted.
func TestAnAPIAgentBridgeIsReachedThroughTheChaperonesGrantedEgress(t *testing.T) {
	t.Parallel()
	tools := agentBridgeProfile("", true)
	tools.Spec.Requires = &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
		Egress: []gentianov1alpha1.EgressRequest{{
			Name:   "tool-server",
			Rule:   &networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.10/32"}}}},
			Reason: "the tool server this bridge offers",
		}},
	}}
	profiles := map[string]*gentianov1alpha1.ComponentProfile{
		"web-tools": tools,
		"chaperone": chaperoneProfile(),
	}

	in := tenantWith(profiles)
	got := byName(netpolicy.BuildDesired(in))
	if _, ok := got["agent-bridge-web-tools"]; ok {
		t.Fatal("an API agent bridge runs no pods, and gets no floor")
	}
	if exprs := got["tenant-isolation"].Spec.PodSelector.MatchExpressions; len(exprs) != 0 {
		t.Fatalf("an API agent bridge has no pods to leave out: %+v", exprs)
	}
	if _, ok := got["agent-bridge-callers-chaperone"]; ok {
		t.Fatal("egress nobody granted reached the chaperone")
	}

	in.Privileges = []gentianov1alpha1.TenantPrivilegeGrant{{
		Install: "web-tools", Privilege: "egress/tool-server", Approver: "perimeter-approver",
		ApprovedAt: metav1.Now(), Reason: "the team uses this search service",
	}}
	got = byName(netpolicy.BuildDesired(in))
	callers := got["agent-bridge-callers-chaperone"]
	if callers == nil || len(callers.Spec.Egress) != 1 || callers.Spec.Egress[0].To[0].IPBlock.CIDR != "203.0.113.10/32" {
		t.Fatalf("the chaperone must reach the granted tool server, got %+v", callers)
	}
}

// A component the platform places that may speak as a person is a caller
// like an installed one: admitted by the bridge and given the way to it,
// selected by the labels its release carries. A placed component that does
// not speak as a person is nobody to an agent bridge.
func TestAPlacedComponentThatSpeaksAsAPersonIsACallerToo(t *testing.T) {
	t.Parallel()
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{
		"openproject":         appProfile(),
		"openproject-adapter": agentBridgeProfile("openproject", false),
	})
	in.Profiles["usher"] = chaperoneProfile()
	in.Profiles["desktop"] = appProfile()
	in.Placed = []string{"usher", "desktop"}
	in.PodSelectors["usher"] = map[string]string{instanceLabel: "tenant-demo-usher"}
	in.PodSelectors["desktop"] = map[string]string{instanceLabel: "tenant-demo-desktop"}
	got := byName(netpolicy.BuildDesired(in))

	floor := got["agent-bridge-openproject-adapter"]
	if floor == nil || len(floor.Spec.Ingress) != 1 || len(floor.Spec.Ingress[0].From) != 1 ||
		floor.Spec.Ingress[0].From[0].PodSelector.MatchLabels[instanceLabel] != "tenant-demo-usher" {
		t.Fatalf("the bridge must admit the placed caller and nobody else, got %+v", floor)
	}
	callers := got["agent-bridge-callers-usher"]
	if callers == nil || callers.Spec.PodSelector.MatchLabels[instanceLabel] != "tenant-demo-usher" ||
		callers.Spec.Egress[0].To[0].PodSelector.MatchLabels[meta.AppLabel] != "openproject-adapter" {
		t.Fatalf("the placed caller must reach the bridge, got %+v", callers)
	}
	if _, ok := got["agent-bridge-callers-desktop"]; ok {
		t.Fatal("a placed component that does not speak as a person was given the way to the bridge")
	}
	if _, ok := netpolicy.ManagedPolicyNames(in)["agent-bridge-callers-usher"]; !ok {
		t.Error("agent-bridge-callers-usher is built and not kept: it would be deleted on the next pass")
	}
	// Nothing else is derived from a placed component here.
	for name := range got {
		if strings.Contains(name, "desktop") {
			t.Errorf("%s was built for a placed component", name)
		}
	}
}

// The tenant's home sync reaches the apps that declare homeSync and the
// providers of the homes, and they admit it; a tenant without one opens
// nothing for it.
func TestTheHomeSyncReachesTheAppsItKeepsEqual(t *testing.T) {
	t.Parallel()
	odoo := appProfile()
	odoo.Spec.HomeSync = &gentianov1alpha1.HomeSyncSpec{}
	home := appProfile()
	home.Spec.Provides = []gentianov1alpha1.ContractRef{{Name: "calendar-store"}}
	in := tenantWith(map[string]*gentianov1alpha1.ComponentProfile{
		"odoo": odoo, "nextcloud": home, "wiki": appProfile(),
	})
	in.Profiles[gentianov1alpha1.HomeSyncProfile] = chaperoneProfile()
	in.Placed = []string{gentianov1alpha1.HomeSyncProfile}
	got := byName(netpolicy.BuildDesired(in))
	out := got["homesync"]
	if out == nil || len(out.Spec.Egress) != 1 || len(out.Spec.Egress[0].To) != 2 {
		t.Fatalf("the home sync's way out: %+v", out)
	}
	for _, target := range []string{"nextcloud", "odoo"} {
		if got["homesync-target-"+target] == nil {
			t.Errorf("%s does not admit the home sync", target)
		}
	}
	if got["homesync-target-wiki"] != nil {
		t.Error("an app the home sync does not call admits it")
	}
	if _, ok := netpolicy.ManagedPolicyNames(in)["homesync"]; !ok {
		t.Error("the home sync's policy is built and not kept")
	}
	in.Placed = nil
	for name := range byName(netpolicy.BuildDesired(in)) {
		if strings.HasPrefix(name, "homesync") {
			t.Errorf("%s built for a tenant without a home sync", name)
		}
	}
}
