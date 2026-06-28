/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing the License.
*/

package netpolicy

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// ContractAllowNetworkPolicy grants consumer→provider traffic for an active
// IntegrationBinding. Today all capabilities on the binding are permitted; a
// future AppGrant reconciler (OpenFGA, Stage 2) will intersect this allowlist.
func ContractAllowNetworkPolicy(tenantName string, binding *gentianov1alpha1.IntegrationBinding) *networkingv1.NetworkPolicy {
	if binding == nil {
		return nil
	}
	consumer := binding.Spec.Consumer.App
	provider := binding.Spec.Provider.App
	if consumer == "" || provider == "" {
		return nil
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      contractPolicyName(binding.Name),
			Namespace: binding.Namespace,
			Labels:    policyLabels(tenantName, meta.NetPolicyContract),
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
