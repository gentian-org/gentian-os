/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// RightsCheckPort is the bouncer's listener for the rights check
// (cmd/bouncer, BOUNCER_CHECK_LISTEN: the two must agree).
const RightsCheckPort = int32(8082)

// ArbiterPort is the arbiter's listener, where a component's authorization
// store is asked and written (internal/arbiter, Port: the two must agree).
const ArbiterPort = int32(8080)

// ArbiterPodLabels are the labels of the arbiter's pods, which the way to it
// selects on (the operator's Deployment of it carries them).
func ArbiterPodLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": "arbiter", "app.kubernetes.io/part-of": "gentian-os"}
}

// RegistrarPort is the registrar's listener (internal/registrar, REGISTRAR_ADDR:
// the two must agree).
const RegistrarPort = int32(9445)

// ContractNetworkPolicies are the two policies of one granted contract: the
// consumer's pods may leave for the provider's, and the provider's pods admit
// the consumer's. Nothing, when the consumer was granted nothing of it.
//
// Both are needed. A tenant's namespace is closed in both directions
// (BaselineNetworkPolicy), so the consumer's way out alone reaches a provider
// that does not answer.
//
// Where the binding carries the provider's endpoint, both are to its port
// only; without one, to the provider's pods as a whole.
//
// The pods are selected by what each app's delivery puts on them, which the
// caller knows and this package does not (selectors); an app it names nothing
// for is selected by the app label, which the operator renders every chart
// it does not place with (podLabels).
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

	// Where the provider names the port it answers the contract on, the
	// way is to that port and no other of its pods.
	var ports []networkingv1.NetworkPolicyPort
	if e := binding.Spec.Endpoint; e != nil && e.Port > 0 {
		tcp := corev1.ProtocolTCP
		port := intstr.FromInt32(e.Port)
		ports = []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}
	}

	return []*networkingv1.NetworkPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Name: contractPolicyName(binding.Name), Namespace: binding.Namespace, Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: consumerPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &providerPods}},
					Ports: ports,
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: contractIngressPolicyName(binding.Name), Namespace: binding.Namespace, Labels: labels},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: providerPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &consumerPods}},
					Ports: ports,
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
