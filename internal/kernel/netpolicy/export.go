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

const exportPolicyNameValue = "gentian-tenant-export"

// ExportJobNetworkPolicy lets backup capture and restore pods do their work.
//
// Volume captures run in the tenant namespace — a PVC is only mountable from
// its own namespace — where the baseline is default-deny plus DNS. Their one
// network need beyond that is the object store the bundle lives in (the infra
// namespace), plus the kernel services namespace for parity with what every
// app is granted. Scoped to component=tenant-export pods so it grants nothing
// to tenant workloads.
func ExportJobNetworkPolicy(tenantName, nsName string, cfg Config) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      exportPolicyNameValue,
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyTenantExport),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				meta.ComponentLabel: "tenant-export",
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				namespaceEgress(cfg.InfraNamespace),
				namespaceEgress(meta.KernelNamespace),
			},
		},
	}
}

func exportPolicyName() string { return exportPolicyNameValue }
