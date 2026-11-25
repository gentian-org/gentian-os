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

	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const baselinePolicyName = "tenant-isolation"

// BaselineNetworkPolicy is the default-deny MAC floor for a tenant namespace.
// Kernel and cross-app access are granted by separate operator-managed policies.
//
// excluded leaves pods out of it: an agent bridge's, which the edge, the
// identity provider and the operator must not reach either, and which have a
// floor of their own instead (AgentBridgeNetworkPolicies). Network rules only ever add, so
// a pod this policy selects could not be closed again by another.
func BaselineNetworkPolicy(tenantName, nsName string, cfg Config, cluster ClusterServices, excluded ...metav1.LabelSelectorRequirement) *networkingv1.NetworkPolicy {
	egress := BaselineEgress(cfg, cluster)

	// The edge: the Gateways' data plane runs in the edge function's
	// namespace, and it is what reaches a tenant's routed pods.
	edge := namespaceIngress(layout.Namespace(layout.Edge))
	if cfg.NarrowEdge {
		// Its Envoy pods and no other pod there: they are what sets the
		// identity headers an app trusts, so cert-manager, external-dns
		// and the bouncer, which share the namespace, have no path to an
		// app on which to say who a caller is.
		edge.From[0].PodSelector = &metav1.LabelSelector{MatchLabels: EdgeProxyPodLabels()}
	}
	ingress := []networkingv1.NetworkPolicyIngressRule{
		edge,
		// Keycloak calls an app's back-channel logout endpoint, so the
		// authentication function may reach tenant pods; nothing else kernel does.
		namespaceIngress(layout.Namespace(layout.Authentication)),
		// The operator itself provisions *into* running tenant apps over their
		// own admin APIs — ComponentProfile.spec.hooks.provisioning.privilegedRole is the
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
			PodSelector: metav1.LabelSelector{MatchExpressions: excluded},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: ingress,
			Egress:  egress,
		},
	}
}

// BaselineEgress is what every pod of a tenant may reach without anybody
// having asked: the cluster's DNS (DNSEgress) and the API server
// (APIServerEgress). An agent bridge's pods are given the same, by their own
// policy, and so are the instances of the model gateway's PostgreSQL
// (internal/controller/model_gateway_database_egress.go).
func BaselineEgress(cfg Config, cluster ClusterServices) []networkingv1.NetworkPolicyEgressRule {
	return append(DNSEgress(cluster.DNS), APIServerEgress(cfg.KubeAPIServerCIDR, cluster)...)
}

// EdgeGatewayClassName is the GatewayClass of the inbound Gateways.
const EdgeGatewayClassName = "gentian-envoy"

// OutgoingGatewayClassName is the GatewayClass of the Gateways for outgoing
// connections, served by a fleet of their own in the edge namespace.
const OutgoingGatewayClassName = "gentian-envoy-out"

// InsideGatewayClassName is the GatewayClass of the Gateway only the
// cluster's own pods reach: the inside entries' fleet, in the edge namespace,
// whose Service has no address outside the cluster.
const InsideGatewayClassName = "gentian-envoy-inside"

// InsideProxyPodLabels select the Envoy proxy pods of the fleet for inside
// entries in the edge namespace, and of no other fleet.
func InsideProxyPodLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":                    "envoy",
		"gateway.envoyproxy.io/owning-gatewayclass": InsideGatewayClassName,
	}
}

// OutgoingProxyPodLabels select the Envoy proxy pods of the fleet for
// outgoing connections in the edge namespace.
func OutgoingProxyPodLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":                    "envoy",
		"gateway.envoyproxy.io/owning-gatewayclass": OutgoingGatewayClassName,
	}
}

// EdgeProxyPodLabels select the Envoy proxy pods of the inbound Gateways in
// the edge namespace, and of no other fleet Envoy Gateway runs there.
//
// The second label names the fleet: Envoy Gateway puts the first on every
// proxy pod of every class, so by the first alone a fleet for another
// direction would be admitted wherever the edge is.
//
// The label is Envoy Gateway's own, on every proxy pod it creates
// (EnvoyAppLabel in its internal/infrastructure/kubernetes/proxy), and the
// one the model gateway's policy and the EnvoyProxy's spread constraint
// already select by. The kernel namespaces' rules name the same pods
// (internal/kernel/kernelnet, peer envoy) and a test there holds the two
// equal.
func EdgeProxyPodLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":                    "envoy",
		"gateway.envoyproxy.io/owning-gatewayclass": EdgeGatewayClassName,
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
