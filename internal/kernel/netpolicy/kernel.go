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
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// KernelAccessNetworkPolicy grants egress from an app workload to kernel namespaces
// declared in its AppProfile kernelRequirements and optional profile annotations.
func KernelAccessNetworkPolicy(
	tenantName, nsName, appName string,
	profile *gentianov1alpha1.AppProfile,
	cfg Config,
) *networkingv1.NetworkPolicy {
	if profile == nil {
		return nil
	}
	targets := kernelEgressTargets(profile.Spec.KernelRequirements, profile, cfg)
	if len(targets) == 0 {
		return nil
	}

	egress := make([]networkingv1.NetworkPolicyEgressRule, 0, len(targets))
	for _, ns := range targets {
		egress = append(egress, namespaceEgress(ns))
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kernelPolicyName(appName),
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyKernel),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{meta.AppLabel: appName},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

func kernelPolicyName(appName string) string {
	name := fmt.Sprintf("kernel-access-%s", appName)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func kernelEgressTargets(kr *gentianov1alpha1.KernelRequirements, profile *gentianov1alpha1.AppProfile, cfg Config) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(ns string) {
		if ns == "" {
			return
		}
		if _, ok := seen[ns]; ok {
			return
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}

	if kr != nil {
		if kr.Identity != nil {
			add(cfg.ServicesNamespace)
			add(meta.KernelNamespace)
		}
		if kr.Database != nil {
			add(cfg.InfraNamespace)
		}
		if kr.Cache != nil {
			add(cfg.InfraNamespace)
		}
		if kr.Storage != nil {
			add(cfg.InfraNamespace)
		}
		if kr.Mail != nil {
			add(cfg.ServicesNamespace)
		}
	}
	for _, ns := range gentianov1alpha1.ProfileKernelEgressNamespaces(profile) {
		add(ns)
	}
	return out
}
