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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gentian-org/gentian-os/internal/meta"
)

const appInitPolicyName = "app-init-access"

// AppInitAccessNetworkPolicy grants bootstrap Jobs (db-init, s3-init) egress to
// OpenBao and kernel namespaces. Init pods carry gentianos.io/component=app-init.
func AppInitAccessNetworkPolicy(tenantName, nsName string, cfg Config) *networkingv1.NetworkPolicy {
	targets := []string{
		cfg.OpenbaoNamespace,
		cfg.InfraNamespace,
		cfg.ServicesNamespace,
		meta.KernelNamespace,
	}
	seen := map[string]struct{}{}
	egress := make([]networkingv1.NetworkPolicyEgressRule, 0, len(targets))
	for _, ns := range targets {
		if ns == "" {
			continue
		}
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		egress = append(egress, namespaceEgress(ns))
	}
	if len(egress) == 0 {
		return nil
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appInitPolicyName,
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyAppInit),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					meta.ComponentLabel: meta.AppInitComponentValue,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}
