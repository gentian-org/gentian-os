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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/gentian-org/gentian-os/internal/layout"
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
				namespaceEgress(layout.System("postgresql")),
				namespaceEgress(layout.System("mariadb")),
				namespaceEgress(layout.System("s3")),
				// Internet, because the encrypt step installs age via apk and
				// a tenant-namespace restore fetches the MinIO client — both
				// at runtime, from public endpoints. Confined to export pods,
				// which are operator-authored workloads; a dedicated backup
				// image with the tools baked in retires this rule, and is the
				// intended successor.
				internetEgress(),
			},
		},
	}
}

func internetEgress() networkingv1.NetworkPolicyEgressRule {
	protocolTCP := corev1.ProtocolTCP
	https := intstr.FromInt32(443)
	http := intstr.FromInt32(80)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}},
		},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &protocolTCP, Port: &https},
			{Protocol: &protocolTCP, Port: &http},
		},
	}
}

func exportPolicyName() string { return exportPolicyNameValue }
