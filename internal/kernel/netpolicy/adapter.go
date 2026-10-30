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

// An adapter turns one app's abilities into tools for a program that acts for
// a person, and is reached by the component that checks every such call and
// by nothing else. That component is the one of the tenant that may speak as a
// person (requires.services.vouching): it is the only one with a reason to
// call a tool, and an adapter called by anything else would act past the
// check. Three rules follow, and all three are derived here from what the
// tenant has installed, with no grant to give:
//
//   - The adapter's pods are left out of the tenant's baseline, which admits
//     the edge, the identity provider and the operator to every pod, and are
//     given a floor of their own: the cluster's DNS and API server out, and in
//     only from the components that speak as a person.
//   - Those components may reach the adapters' pods, and, for an adapter that
//     is an API outside the cluster, whatever egress a person granted that
//     adapter: nothing runs for it in the tenant, so the way out is the
//     caller's.
//   - An adapter reaches the app it adapts, and that app admits it. The app is
//     not asked to cooperate: installing the adapter is the decision.

const (
	adapterPolicyPrefix        = "adapter-"
	adapterCallersPolicyPrefix = "adapter-callers-"
	adapterTargetPolicyPrefix  = "adapter-target-"
)

func adapterPolicyName(adapter string) string { return trimPolicyName(adapterPolicyPrefix + adapter) }
func adapterCallersPolicyName(caller string) string {
	return trimPolicyName(adapterCallersPolicyPrefix + caller)
}
func adapterTargetPolicyName(adapter string) string {
	return trimPolicyName(adapterTargetPolicyPrefix + adapter)
}

func trimPolicyName(name string) string {
	if len(name) > 63 {
		return name[:63]
	}
	return name
}

// isAdapter reports whether a profile is an adapter's.
func isAdapter(profile *gentianov1alpha1.ComponentProfile) bool {
	if profile == nil {
		return false
	}
	for _, c := range profile.Spec.Classes {
		if c == gentianov1alpha1.ComponentClassAdapter {
			return true
		}
	}
	return false
}

// speaksAsPerson reports whether a profile may speak as a person for the apps
// of its tenant: the one kind of component an adapter admits.
func speaksAsPerson(profile *gentianov1alpha1.ComponentProfile) bool {
	return profile != nil && profile.Spec.Requires != nil && profile.Spec.Requires.Services != nil &&
		profile.Spec.Requires.Services.Vouching != nil
}

// adapterRoles are the installed adapters and the installed components that
// may call them, each by install name, sorted and without repeats.
func (in BuildInput) adapterRoles() (adapters, callers []string) {
	seen := map[string]struct{}{}
	for _, app := range in.Apps {
		if _, done := seen[app.Profile]; done {
			continue
		}
		seen[app.Profile] = struct{}{}
		profile := in.Profiles[app.Profile]
		switch {
		case isAdapter(profile):
			adapters = append(adapters, app.Profile)
		case speaksAsPerson(profile):
			callers = append(callers, app.Profile)
		}
	}
	sort.Strings(adapters)
	sort.Strings(callers)
	return adapters, callers
}

// installed reports whether an app is installed in the tenant.
func (in BuildInput) installed(name string) bool {
	for _, app := range in.Apps {
		if app.Profile == name {
			return true
		}
	}
	return false
}

// deploysPods reports whether an install runs anything in the tenant. An
// adapter packaged as an API is reached where it is, outside the cluster.
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

// adapterExclusion is what leaves the adapters' pods out of the baseline: for
// each label an adapter's pods are selected by, the pods that carry another
// value of it, or none. Each install is selected by one label, so a pod is
// left out exactly when it carries an adapter's.
func (in BuildInput) adapterExclusion() []metav1.LabelSelectorRequirement {
	adapters, _ := in.adapterRoles()
	values := map[string][]string{}
	for _, adapter := range adapters {
		if !deploysPods(in.Profiles[adapter]) {
			continue
		}
		for key, value := range in.podsOf(adapter).MatchLabels {
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

// AdapterNetworkPolicies are the rules for a tenant's adapters: each one's
// floor, the way of the components that call them, and the way into the apps
// they adapt.
func AdapterNetworkPolicies(in BuildInput) []*networkingv1.NetworkPolicy {
	adapters, callers := in.adapterRoles()
	if len(adapters) == 0 {
		return nil
	}
	labels := func() map[string]string { return policyLabels(in.TenantName, meta.NetPolicyAdapter) }
	var out []*networkingv1.NetworkPolicy

	callerPeers := make([]networkingv1.NetworkPolicyPeer, 0, len(callers))
	for _, caller := range callers {
		pods := in.podsOf(caller)
		callerPeers = append(callerPeers, networkingv1.NetworkPolicyPeer{PodSelector: &pods})
	}

	var adapterPeers []networkingv1.NetworkPolicyPeer
	var outside []networkingv1.NetworkPolicyEgressRule
	for _, adapter := range adapters {
		profile := in.Profiles[adapter]
		if !deploysPods(profile) {
			// Reached where it is. The way there is its granted egress, and
			// it is the callers' to take.
			outside = append(outside, in.grantedEgress(adapter, profile)...)
			continue
		}
		pods := in.podsOf(adapter)
		adapterPeers = append(adapterPeers, networkingv1.NetworkPolicyPeer{PodSelector: &pods})

		egress := baselineEgress(in.Config, in.KubeAPIEndpts)
		var ingress []networkingv1.NetworkPolicyIngressRule
		if len(callerPeers) > 0 {
			ingress = []networkingv1.NetworkPolicyIngressRule{{From: callerPeers}}
		}
		if target := adaptedApp(profile); target != "" && target != adapter && in.installed(target) {
			app := in.podsOf(target)
			egress = append(egress, networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{{PodSelector: &app}},
			})
			out = append(out, &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: adapterTargetPolicyName(adapter), Namespace: in.Namespace, Labels: labels()},
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
			ObjectMeta: metav1.ObjectMeta{Name: adapterPolicyName(adapter), Namespace: in.Namespace, Labels: labels()},
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

	if len(adapterPeers) == 0 && len(outside) == 0 {
		return out
	}
	var toAdapters []networkingv1.NetworkPolicyEgressRule
	if len(adapterPeers) > 0 {
		toAdapters = append(toAdapters, networkingv1.NetworkPolicyEgressRule{To: adapterPeers})
	}
	toAdapters = append(toAdapters, outside...)
	for _, caller := range callers {
		out = append(out, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: adapterCallersPolicyName(caller), Namespace: in.Namespace, Labels: labels()},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: in.podsOf(caller),
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress:      toAdapters,
			},
		})
	}
	return out
}

// adaptedApp is the install name of the app an adapter adapts, or "".
func adaptedApp(profile *gentianov1alpha1.ComponentProfile) string {
	if profile == nil || profile.Spec.Adapter == nil || profile.Spec.Adapter.Adapts == nil {
		return ""
	}
	return profile.Spec.Adapter.Adapts.Profile
}
