/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"net"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DNSNamespace is where a cluster's DNS Service is looked for.
const DNSNamespace = metav1.NamespaceSystem

// DNSPort is the port a DNS Service serves on.
const DNSPort int32 = 53

// APIServerServiceName is the API server's Service, in the default namespace.
const APIServerServiceName = "kubernetes"

// ClusterServices are the cluster's own services every tenant pod may reach,
// as the operator reads them: its DNS and the API server. No label names
// either on every distribution (CoreDNS is k8s-app=coredns on one,
// k8s-app=kube-dns on another), so both are read from their Services.
type ClusterServices struct {
	// DNS are the Services in kube-system that serve port 53 (ServesDNS).
	// Empty when none was found: port 53 is then open to every address
	// (DNSEgress).
	DNS []corev1.Service
	// APIService is the kubernetes Service in default; nil when unreadable.
	APIService *corev1.Service
	// APIEndpoints are the EndpointSlices of the kubernetes Service, which
	// name the addresses the API server answers on.
	APIEndpoints []discoveryv1.EndpointSlice
}

// ServesDNS reports whether a Service is one of the cluster's DNS: in
// kube-system, serving port 53, and selecting its pods. A Service without a
// selector names no pods a rule could select and is not taken.
func ServesDNS(svc *corev1.Service) bool {
	if svc.Namespace != DNSNamespace || len(svc.Spec.Selector) == 0 {
		return false
	}
	for _, p := range svc.Spec.Ports {
		if p.Port == DNSPort {
			return true
		}
	}
	return false
}

// HasDNS reports whether the cluster's DNS was found, so port 53 is narrowed
// to it.
func (c ClusterServices) HasDNS() bool {
	for i := range c.DNS {
		if ServesDNS(&c.DNS[i]) {
			return true
		}
	}
	return false
}

// HasAPIEndpoints reports whether an address of the API server was read, so
// the fallback range is not needed.
func (c ClusterServices) HasAPIEndpoints() bool {
	for i := range c.APIEndpoints {
		for _, ep := range c.APIEndpoints[i].Endpoints {
			if len(ep.Addresses) > 0 {
				return true
			}
		}
	}
	return false
}

// DNSEgress is the way to the cluster's DNS. For each DNS Service: its pods,
// by the Service's own selector in kube-system, on the ports the Service
// sends 53 to; and the Service's cluster address on 53. A datapath that
// evaluates a policy after the Service address is translated (Calico) sees
// the pod, one that evaluates it before sees the cluster address, and the
// two rules hold in either order.
//
// With no DNS Service found, port 53 is open to every address: a tenant
// whose pods cannot resolve a name cannot run at all, and a policy is not
// what should take a cluster's tenants down. The operator logs the refusal
// to narrow (loadClusterServices in internal/controller).
func DNSEgress(dns []corev1.Service) []networkingv1.NetworkPolicyEgressRule {
	var out []networkingv1.NetworkPolicyEgressRule
	for i := range dns {
		svc := &dns[i]
		if !ServesDNS(svc) {
			continue
		}
		var targets, ports []networkingv1.NetworkPolicyPort
		for _, p := range svc.Spec.Ports {
			if p.Port != DNSPort {
				continue
			}
			protocol := p.Protocol
			if protocol == "" {
				protocol = corev1.ProtocolTCP
			}
			target := p.TargetPort
			if target.Type == intstr.Int && target.IntVal == 0 {
				target = intstr.FromInt32(p.Port)
			}
			port := intstr.FromInt32(p.Port)
			targets = append(targets, networkingv1.NetworkPolicyPort{Protocol: ptrTo(protocol), Port: &target})
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: ptrTo(protocol), Port: &port})
		}
		selector := make(map[string]string, len(svc.Spec.Selector))
		for k, v := range svc.Spec.Selector {
			selector[k] = v
		}
		out = append(out, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": svc.Namespace,
				}},
				PodSelector: &metav1.LabelSelector{MatchLabels: selector},
			}},
			Ports: targets,
		})
		if peers := clusterIPPeers(svc); len(peers) > 0 {
			out = append(out, networkingv1.NetworkPolicyEgressRule{To: peers, Ports: ports})
		}
	}
	if len(out) == 0 {
		dnsPort := intstr.FromInt32(DNSPort)
		out = append(out, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: ptrTo(corev1.ProtocolUDP), Port: &dnsPort},
				{Protocol: ptrTo(corev1.ProtocolTCP), Port: &dnsPort},
			},
		})
	}
	return out
}

// APIServerEgress is the way to the API server: each address and port of
// the kubernetes Service's EndpointSlices, and the Service's cluster address
// on its own ports, so a datapath that evaluates a policy after the Service
// address is translated and one that evaluates it before both let it
// through.
//
// fallbackCIDR (KUBE_APISERVER_CIDR) is admitted on 443 only when no address
// of the API server was read: the cluster address alone does not reach it
// where the policy is evaluated after translation.
func APIServerEgress(fallbackCIDR string, cluster ClusterServices) []networkingv1.NetworkPolicyEgressRule {
	var out []networkingv1.NetworkPolicyEgressRule
	if !cluster.HasAPIEndpoints() && fallbackCIDR != "" {
		port := intstr.FromInt32(443)
		out = append(out, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: fallbackCIDR}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptrTo(corev1.ProtocolTCP), Port: &port}},
		})
	}
	for i := range cluster.APIEndpoints {
		slice := &cluster.APIEndpoints[i]
		for _, ep := range slice.Endpoints {
			for _, addr := range ep.Addresses {
				cidr := hostCIDR(addr)
				if cidr == "" {
					continue
				}
				for _, p := range slice.Ports {
					if p.Protocol == nil || *p.Protocol != corev1.ProtocolTCP || p.Port == nil {
						continue
					}
					port := intstr.FromInt32(*p.Port)
					out = append(out, networkingv1.NetworkPolicyEgressRule{
						To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}},
						Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptrTo(corev1.ProtocolTCP), Port: &port}},
					})
				}
			}
		}
	}
	if svc := cluster.APIService; svc != nil {
		var ports []networkingv1.NetworkPolicyPort
		for _, p := range svc.Spec.Ports {
			if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP {
				continue
			}
			port := intstr.FromInt32(p.Port)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: ptrTo(corev1.ProtocolTCP), Port: &port})
		}
		if peers := clusterIPPeers(svc); len(peers) > 0 && len(ports) > 0 {
			out = append(out, networkingv1.NetworkPolicyEgressRule{To: peers, Ports: ports})
		}
	}
	return out
}

// clusterIPPeers are a Service's cluster addresses, one host block each. A
// headless Service has none.
func clusterIPPeers(svc *corev1.Service) []networkingv1.NetworkPolicyPeer {
	ips := svc.Spec.ClusterIPs
	if len(ips) == 0 && svc.Spec.ClusterIP != "" {
		ips = []string{svc.Spec.ClusterIP}
	}
	var out []networkingv1.NetworkPolicyPeer
	for _, ip := range ips {
		if cidr := hostCIDR(ip); cidr != "" {
			out = append(out, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
		}
	}
	return out
}

// hostCIDR is one address as a block of one: /32 for IPv4, /128 for IPv6, ""
// for what is no address ("None" of a headless Service).
func hostCIDR(addr string) string {
	ip := net.ParseIP(addr)
	switch {
	case ip == nil:
		return ""
	case ip.To4() != nil:
		return ip.String() + "/32"
	default:
		return ip.String() + "/128"
	}
}

func ptrTo[T any](v T) *T { return &v }
