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

// ContractAllowNetworkPolicy grants consumer→provider traffic for an active
// IntegrationBinding when AppGrant (if present) allows at least one capability.
func ContractAllowNetworkPolicy(
	tenantName string,
	binding *gentianov1alpha1.IntegrationBinding,
	grant *gentianov1alpha1.AppGrant,
) *networkingv1.NetworkPolicy {
	if binding == nil {
		return nil
	}
	consumer := binding.Spec.Consumer.App
	provider := binding.Spec.Provider.App
	if consumer == "" || provider == "" {
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

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      contractPolicyName(binding.Name),
			Namespace: binding.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{meta.AppLabel: consumer},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{meta.AppLabel: provider},
							},
						},
					},
				},
			},
		},
	}
}

func contractPolicyName(bindingName string) string {
	name := "contract-" + bindingName
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
