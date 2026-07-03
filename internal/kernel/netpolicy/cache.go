/*
Copyright 2026 The Gentian Authors.

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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gentian-org/gentian-os/internal/meta"
)

const tenantCachePolicyPrefix = "tenant-cache-"

// TenantCacheEgressNetworkPolicy allows app workloads that declare a cache
// kernel requirement to reach the shared tenant Memcached instance.
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
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{cachePeer}}},
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
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: appPeers}},
		},
	}
}

func tenantCacheEgressPolicyName() string {
	return tenantCachePolicyPrefix + "egress"
}

func tenantCacheIngressPolicyName() string {
	return tenantCachePolicyPrefix + "ingress"
}
