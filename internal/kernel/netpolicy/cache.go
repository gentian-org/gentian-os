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

	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const tenantCachePolicyPrefix = "tenant-cache-"

// TenantCacheEgressNetworkPolicy allows app workloads that declare a
// Memcached cache to reach the tenant's Memcached instance, on its port. An
// app that declared Redis is not among them: its cache is the shared Redis.
func TenantCacheEgressNetworkPolicy(tenantName, nsName string, cacheAppNames []string) *networkingv1.NetworkPolicy {
	if len(cacheAppNames) == 0 {
		return nil
	}
	cachePeer := networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{meta.ComponentLabel: meta.TenantCacheComponentValue},
		},
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantCacheEgressPolicyName(),
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyTenantCache),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      meta.AppLabel,
					Operator: metav1.LabelSelectorOpIn,
					Values:   cacheAppNames,
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{cachePeer},
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(provisioner.MemcachedPort)},
			}},
		},
	}
}

// TenantCacheIngressNetworkPolicy allows the shared tenant Memcached instance to
// accept traffic from cache-consuming app workloads.
func TenantCacheIngressNetworkPolicy(tenantName, nsName string, cacheAppNames []string) *networkingv1.NetworkPolicy {
	if len(cacheAppNames) == 0 {
		return nil
	}
	appPeers := make([]networkingv1.NetworkPolicyPeer, 0, len(cacheAppNames))
	for _, appName := range cacheAppNames {
		appPeers = append(appPeers, networkingv1.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{meta.AppLabel: appName},
			},
		})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tenantCacheIngressPolicyName(),
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyTenantCache),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{meta.ComponentLabel: meta.TenantCacheComponentValue},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  appPeers,
				Ports: []networkingv1.NetworkPolicyPort{tcpPort(provisioner.MemcachedPort)},
			}},
		},
	}
}

func tenantCacheEgressPolicyName() string {
	return tenantCachePolicyPrefix + "egress"
}

func tenantCacheIngressPolicyName() string {
	return tenantCachePolicyPrefix + "ingress"
}
