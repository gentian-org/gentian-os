/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package netpolicy

import (
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/gentian-org/gentian-os/internal/meta"
)

const baselinePolicyName = "tenant-isolation"

// BaselineNetworkPolicy is the default-deny MAC floor for a tenant namespace.
// Kernel and cross-app access are granted by separate operator-managed policies.
func BaselineNetworkPolicy(tenantName, nsName string, cfg Config, kubeAPIEndpts *discoveryv1.EndpointSlice) *networkingv1.NetworkPolicy {
	protocolTCP := corev1.ProtocolTCP
	protocolUDP := corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)
	apiServerPort := intstr.FromInt32(443)

	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &protocolUDP, Port: &dnsPort},
				{Protocol: &protocolTCP, Port: &dnsPort},
			},
		},
	}
	if cfg.KubeAPIServerCIDR != "" {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: cfg.KubeAPIServerCIDR}},
			},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &protocolTCP, Port: &apiServerPort},
			},
		})
	}
	if kubeAPIEndpts != nil {
		for _, ep := range kubeAPIEndpts.Endpoints {
			for _, addr := range ep.Addresses {
				for _, port := range kubeAPIEndpts.Ports {
					if port.Protocol == nil || *port.Protocol != corev1.ProtocolTCP || port.Port == nil {
						continue
					}
					endpointPort := intstr.FromInt32(*port.Port)
					egress = append(egress, networkingv1.NetworkPolicyEgressRule{
						To: []networkingv1.NetworkPolicyPeer{
							{IPBlock: &networkingv1.IPBlock{CIDR: addr + "/32"}},
						},
						Ports: []networkingv1.NetworkPolicyPort{
							{Protocol: &protocolTCP, Port: &endpointPort},
						},
					})
				}
			}
		}
	}

	ingress := []networkingv1.NetworkPolicyIngressRule{
		namespaceIngress(meta.EnvoyGatewayInstallNamespace),
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      baselinePolicyName,
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyBaseline),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: ingress,
			Egress:  egress,
		},
	}
}

func namespaceIngress(ns string) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
			}},
		},
	}
}

func namespaceEgress(ns string) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
			}},
		},
	}
}

func policyLabels(tenantName, policyType string) map[string]string {
	return map[string]string{
		meta.TenantLabel:       tenantName,
		meta.ManagedByLabel:      meta.ManagedByValue,
		meta.NetPolicyTypeLabel:  policyType,
	}
}
