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

const appInternalPolicyPrefix = "app-internal-"

// AppInternalAccessNetworkPolicy allows pods carrying gentianos.io/app=<profile>
// to reach each other within the tenant namespace (e.g. synapse-web → synapse).
func AppInternalAccessNetworkPolicy(tenantName, nsName, appName string) *networkingv1.NetworkPolicy {
	appPeer := networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{meta.AppLabel: appName},
		},
	}
	name := appInternalPolicyName(appName)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyAppInternal),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{meta.AppLabel: appName},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{appPeer}}},
			Egress:  []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{appPeer}}},
		},
	}
}

func appInternalPolicyName(appName string) string {
	name := appInternalPolicyPrefix + appName
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
