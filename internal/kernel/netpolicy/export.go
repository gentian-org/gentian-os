/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"

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
//
// No way out of the cluster: a run whose bundle is at an external destination
// is given its own, to that destination alone, for as long as it runs
// (ExportDestinationNetworkPolicy).
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
			},
		},
	}
}

// ExportDestinationNetworkPolicy opens the way from one export's or
// restore's pods in the tenant's namespace to the external destination its
// bundle is at, and nowhere else: the destination's port, and, when its
// endpoint names an address, that address alone. runLabel is the label
// value the run's pods carry (backup.ExportLabel); name is the policy's.
//
// Nil for the platform's own storage (an empty endpoint): that is reached
// through ExportJobNetworkPolicy, and a run whose bundle stays in the
// cluster gets no way out of it.
//
// An endpoint that names a host by its DNS name can only be given the port,
// to any address: a NetworkPolicy matches addresses, not names, and a rule
// by name needs a CNI's own extension (an FQDN policy), which the platform
// does not assume.
func ExportDestinationNetworkPolicy(tenantName, nsName, name, runLabelKey, runLabel, endpoint string) (*networkingv1.NetworkPolicy, error) {
	if endpoint == "" {
		return nil, nil
	}
	peer, port, err := destinationPeer(endpoint)
	if err != nil {
		return nil, err
	}
	protocolTCP := corev1.ProtocolTCP
	p := intstr.FromInt32(port)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyExportDestination),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				meta.ComponentLabel: "tenant-export",
				runLabelKey:         runLabel,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{peer},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocolTCP, Port: &p}},
			}},
		},
	}, nil
}

// destinationPeer is where an endpoint is, as a NetworkPolicy can say it:
// the address when the endpoint names one, any address when it names a host
// by name; and the port, the endpoint's own or its scheme's.
func destinationPeer(endpoint string) (networkingv1.NetworkPolicyPeer, int32, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return networkingv1.NetworkPolicyPeer{}, 0, fmt.Errorf("the destination %q is not a URL with a host", endpoint)
	}
	var port int32
	switch {
	case u.Port() != "":
		n, err := strconv.ParseInt(u.Port(), 10, 32)
		if err != nil || n < 1 || n > 65535 {
			return networkingv1.NetworkPolicyPeer{}, 0, fmt.Errorf("the destination %q names no usable port", endpoint)
		}
		port = int32(n)
	case u.Scheme == "https":
		port = 443
	case u.Scheme == "http":
		port = 80
	default:
		return networkingv1.NetworkPolicyPeer{}, 0, fmt.Errorf("the destination %q is neither http nor https", endpoint)
	}
	cidr := "0.0.0.0/0"
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		cidr = netip.PrefixFrom(ip, ip.BitLen()).String()
	}
	return networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}, port, nil
}

func exportPolicyName() string { return exportPolicyNameValue }
