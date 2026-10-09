/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// RightsCheckPort is the bouncer's listener for the rights check
// (cmd/bouncer, BOUNCER_CHECK_LISTEN: the two must agree).
const RightsCheckPort = int32(8082)

// ContractNetworkPolicies are the two policies of one granted contract: the
// consumer's pods may leave for the provider's, and the provider's pods admit
// the consumer's. Nothing, when the consumer was granted nothing of it.
//
// Both are needed. A tenant's namespace is closed in both directions
// (BaselineNetworkPolicy), so the consumer's way out alone reaches a provider
// that does not answer.
//
// The pods are selected by what each app's delivery puts on them, which the
// caller knows and this package does not (selectors); an app it names nothing
// for is selected by the app label, which is the app Composition's.
func ContractNetworkPolicies(
	tenantName string,
	binding *gentianov1alpha1.IntegrationBinding,
	grant *gentianov1alpha1.AppGrant,
	selectors map[string]map[string]string,
) []*networkingv1.NetworkPolicy {
	if binding == nil {
		return nil
	}
	consumer := binding.Spec.Consumer.App
	provider := binding.Spec.Provider.App
	if consumer == "" || provider == "" || consumer == provider {
		return nil
	}
	effective := EffectiveContractCapabilities(binding, grant)
	if len(effective) == 0 {
		return nil
	}

	labels := policyLabels(tenantName, meta.NetPolicyContract)
	if capLabel := FormatCapabilityLabel(effective); capLabel != "" {
		labels["gentianos.io/granted-capabilities"] = capLabel
	}
	selector := func(app string) metav1.LabelSelector {
		if s := selectors[app]; len(s) > 0 {
			return metav1.LabelSelector{MatchLabels: s}
		}
		return metav1.LabelSelector{MatchLabels: map[string]string{meta.AppLabel: app}}
	}
	consumerPods, providerPods := selector(consumer), selector(provider)

	return []*networkingv1.NetworkPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Name: contractPolicyName(binding.Name), Namespace: binding.Namespace, Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: consumerPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{PodSelector: &providerPods}},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: contractIngressPolicyName(binding.Name), Namespace: binding.Namespace, Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: providerPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{PodSelector: &consumerPods}},
				}},
			},
		},
	}
}

func contractIngressPolicyName(bindingName string) string {
	name := "contract-in-" + bindingName
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func contractPolicyName(bindingName string) string {
	name := "contract-" + bindingName
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
