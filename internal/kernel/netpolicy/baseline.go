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
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/gentian-org/gentian-os/internal/layout"
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
		// The edge: the Gateways' data plane runs in the edge function's
		// namespace, and it is what reaches a tenant's routed pods.
		namespaceIngress(layout.Namespace(layout.Edge)),
		// Keycloak calls an app's back-channel logout endpoint, so the
		// authentication function may reach tenant pods; nothing else kernel does.
		namespaceIngress(layout.Namespace(layout.Authentication)),
		// The operator itself provisions *into* running tenant apps over their
		// own admin APIs — AppProfile.spec.provisioning.privilegedRole is the
		// first such case (see app_privilege_reconciler.go). It runs in
		// OperatorNamespace, not KernelNamespace, so without this it cannot
		// reach the very workloads it reconciles. This grants no new authority:
		// the operator already has API-level control over these namespaces.
		namespaceIngress(meta.OperatorNamespace),
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

// namespacePeer selects a namespace by its immutable metadata.name label.
//
// The ingress and egress rules below are separate types with differently named
// fields, so they cannot be one function — but the peer they both carry can be,
// and it is the part that would be wrong in only one of them.
func namespacePeer(ns string) []networkingv1.NetworkPolicyPeer {
	return []networkingv1.NetworkPolicyPeer{
		{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
		}},
	}
}

func namespaceIngress(ns string) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{From: namespacePeer(ns)}
}

func namespaceEgress(ns string) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{To: namespacePeer(ns)}
}

func policyLabels(tenantName, policyType string) map[string]string {
	return map[string]string{
		meta.TenantLabel:        tenantName,
		meta.ManagedByLabel:     meta.ManagedByValue,
		meta.NetPolicyTypeLabel: policyType,
	}
}
