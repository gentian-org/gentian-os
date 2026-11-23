/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// An agent bridge turns one app's abilities, or an outside or partner's
// service, into tools for a program that acts for a person, and is reached
// by the component that checks every such call and by nothing else. That
// component is the one of the tenant that may speak as a person
// (requires.services.vouching): it is the only one with a reason to call a
// tool, and a bridge called by anything else would act past the check. Three
// rules follow, and all three are derived here from what the tenant has
// installed, with no grant to give:
//
//   - The bridge's pods are left out of the tenant's baseline, which admits
//     the edge, the identity provider and the operator to every pod, and are
//     given a floor of their own: the cluster's DNS and API server out, and in
//     only from the components that speak as a person.
//   - Those components may reach the bridges' pods, and, for a bridge that
//     is an API outside the cluster, whatever egress a person granted that
//     bridge: nothing runs for it in the tenant, so the way out is the
//     caller's.
//   - A bridge reaches the app it connects, and that app admits it. The app
//     is not asked to cooperate: installing the bridge is the decision.
//
// A bridge with no way from the agents -- a sync bridge, reached at the
// door -- is none of this file's.

const (
	agentBridgePolicyPrefix        = "agent-bridge-"
	agentBridgeCallersPolicyPrefix = "agent-bridge-callers-"
	agentBridgeTargetPolicyPrefix  = "agent-bridge-target-"
)

func agentBridgePolicyName(bridge string) string {
	return trimPolicyName(agentBridgePolicyPrefix + bridge)
}
func agentBridgeCallersPolicyName(caller string) string {
	return trimPolicyName(agentBridgeCallersPolicyPrefix + caller)
}
func agentBridgeTargetPolicyName(bridge string) string {
	return trimPolicyName(agentBridgeTargetPolicyPrefix + bridge)
}

func trimPolicyName(name string) string {
	if len(name) > 63 {
		return name[:63]
	}
	return name
}

// isAgentBridge reports whether a profile is an agent bridge's: a bridge
// with a way from the tenant's agents.
func isAgentBridge(profile *gentianov1alpha1.ComponentProfile) bool {
	if profile.AgentWay() == nil {
		return false
	}
	for _, c := range profile.Spec.Classes {
		if c == gentianov1alpha1.ComponentClassBridge {
			return true
		}
	}
	return false
}

// speaksAsPerson reports whether a profile may speak as a person for the apps
// of its tenant: the one kind of component an agent bridge admits.
func speaksAsPerson(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile != nil && profile.Spec.Requires != nil && profile.Spec.Requires.Services != nil &&
		profile.Spec.Requires.Services.Vouching != nil
}

// agentBridgeRoles are the installed agent bridges and the components that
// may call them, each by install name, sorted and without repeats. A caller
// is installed or placed by the platform: what admits it is that it may
// speak as a person, not who put it there. A bridge is always installed.
func (in BuildInput) agentBridgeRoles() (bridges, callers []string) {
	seen := map[string]struct{}{}
	for _, app := range in.Apps {
		if _, done := seen[app.Profile]; done {
			continue
		}
		seen[app.Profile] = struct{}{}
		profile := in.Profiles[app.Profile]
		switch {
		case isAgentBridge(profile):
			bridges = append(bridges, app.Profile)
		case speaksAsPerson(profile):
			callers = append(callers, app.Profile)
		}
	}
	for _, name := range in.Placed {
		if _, done := seen[name]; done {
			continue
		}
		seen[name] = struct{}{}
		if speaksAsPerson(in.Profiles[name]) {
			callers = append(callers, name)
		}
	}
	sort.Strings(bridges)
	sort.Strings(callers)
	return bridges, callers
}

// AgentBridgeRoles are the tenant's installed agent bridges and the
// components that may call them, as the bridges' policies have them.
func AgentBridgeRoles(in BuildInput) (bridges, callers []string) { return in.agentBridgeRoles() }

// installed reports whether an app is installed in the tenant.
func (in BuildInput) installed(name string) bool {
	for _, app := range in.Apps {
		if app.Profile == name {
			return true
		}
	}
	return false
}

// deploysPods reports whether an install runs anything in the tenant. A
// bridge packaged as an API is reached where it is.
func deploysPods(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile != nil && profile.Spec.Package.API == nil
}

// podsOf selects the pods of one install: by the labels its release carries
// where the platform places it, by the app label otherwise.
func (in BuildInput) podsOf(app string) metav1.LabelSelector {
	if s := in.PodSelectors[app]; len(s) > 0 {
		return metav1.LabelSelector{MatchLabels: s}
	}
	return metav1.LabelSelector{MatchLabels: map[string]string{meta.AppLabel: app}}
}

// agentBridgeExclusion is what leaves the agent bridges' pods out of the
// baseline: for each label a bridge's pods are selected by, the pods that
// carry another value of it, or none. Each install is selected by one label,
// so a pod is left out exactly when it carries a bridge's.
func (in BuildInput) agentBridgeExclusion() []metav1.LabelSelectorRequirement {
	bridges, _ := in.agentBridgeRoles()
	values := map[string][]string{}
	for _, bridge := range bridges {
		if !deploysPods(in.Profiles[bridge]) {
			continue
		}
		for key, value := range in.podsOf(bridge).MatchLabels {
			values[key] = append(values[key], value)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]metav1.LabelSelectorRequirement, 0, len(keys))
	for _, key := range keys {
		vals := values[key]
		sort.Strings(vals)
		out = append(out, metav1.LabelSelectorRequirement{Key: key, Operator: metav1.LabelSelectorOpNotIn, Values: vals})
	}
	return out
}

// AgentBridgeNetworkPolicies are the rules for a tenant's agent bridges:
// each one's floor, the way of the components that call them, and the way
// into the apps they connect.
func AgentBridgeNetworkPolicies(in BuildInput) []*networkingv1.NetworkPolicy {
	bridges, callers := in.agentBridgeRoles()
	if len(bridges) == 0 {
		return nil
	}
	labels := func() map[string]string { return policyLabels(in.TenantName, meta.NetPolicyAgentBridge) }
	var out []*networkingv1.NetworkPolicy

	callerPeers := make([]networkingv1.NetworkPolicyPeer, 0, len(callers))
	for _, caller := range callers {
		pods := in.podsOf(caller)
		callerPeers = append(callerPeers, networkingv1.NetworkPolicyPeer{PodSelector: &pods})
	}

	var bridgePeers []networkingv1.NetworkPolicyPeer
	var outside []networkingv1.NetworkPolicyEgressRule
	for _, bridge := range bridges {
		profile := in.Profiles[bridge]
		if !deploysPods(profile) {
			// Reached where it is. The way there is its granted egress, and
			// it is the callers' to take.
			outside = append(outside, in.grantedEgress(bridge, profile)...)
			continue
		}
		pods := in.podsOf(bridge)
		bridgePeers = append(bridgePeers, networkingv1.NetworkPolicyPeer{PodSelector: &pods})

		egress := BaselineEgress(in.Config, in.Cluster)
		var ingress []networkingv1.NetworkPolicyIngressRule
		if len(callerPeers) > 0 {
			ingress = []networkingv1.NetworkPolicyIngressRule{{From: callerPeers}}
		}
		if target := profile.BridgedAppName(); target != "" && target != bridge && in.installed(target) {
			app := in.podsOf(target)
			egress = append(egress, networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{{PodSelector: &app}},
			})
			out = append(out, &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: agentBridgeTargetPolicyName(bridge), Namespace: in.Namespace, Labels: labels()},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: app,
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					Ingress: []networkingv1.NetworkPolicyIngressRule{{
						From: []networkingv1.NetworkPolicyPeer{{PodSelector: &pods}},
					}},
				},
			})
		}
		out = append(out, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: agentBridgePolicyName(bridge), Namespace: in.Namespace, Labels: labels()},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: pods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				// Only the components that speak as a person. None installed,
				// none admitted.
				Ingress: ingress,
				Egress:  egress,
			},
		})
	}

	if len(bridgePeers) == 0 && len(outside) == 0 {
		return out
	}
	var toBridges []networkingv1.NetworkPolicyEgressRule
	if len(bridgePeers) > 0 {
		toBridges = append(toBridges, networkingv1.NetworkPolicyEgressRule{To: bridgePeers})
	}
	toBridges = append(toBridges, outside...)
	for _, caller := range callers {
		out = append(out, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: agentBridgeCallersPolicyName(caller), Namespace: in.Namespace, Labels: labels()},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: in.podsOf(caller),
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress:      toBridges,
			},
		})
	}
	return out
}
