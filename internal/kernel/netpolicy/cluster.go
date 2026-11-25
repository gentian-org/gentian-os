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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// DNS is the cluster's DNS (ClusterDNS).
	DNS ClusterDNS
	// APIService is the kubernetes Service in default; nil when unreadable.
	APIService *corev1.Service
	// APIEndpoints are the EndpointSlices of the kubernetes Service, which
	// name the addresses the API server answers on.
	APIEndpoints []discoveryv1.EndpointSlice
}

// ClusterDNS is the cluster's DNS as a pod's lookup reaches it.
type ClusterDNS struct {
	// Services are the Services in kube-system that serve port 53
	// (ServesDNS). Empty when none was found: port 53 is then open to every
	// address (DNSEgress).
	Services []corev1.Service
	// Redirects are the node-local caches the traffic for one of Services is
	// sent to instead (LocalRedirects).
	Redirects []LocalRedirect
}

// LocalRedirect is a set of pods that traffic for a DNS Service is redirected
// to on the sending pod's own node, before any policy sees it: a node-local
// DNS cache behind Cilium's CiliumLocalRedirectPolicy. A policy is evaluated
// against the cache pod, so the cache pod is what a rule has to admit.
type LocalRedirect struct {
	// Namespace is where the pods are: the redirect policy's own.
	Namespace string
	// Selector picks the pods in Namespace.
	Selector metav1.LabelSelector
	// Ports are the ports the traffic arrives on at the pods.
	Ports []networkingv1.NetworkPolicyPort
}

// LocalRedirectPolicyGVK is Cilium's CiliumLocalRedirectPolicy, read as
// unstructured so the operator does not depend on Cilium's API.
var LocalRedirectPolicyGVK = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumLocalRedirectPolicy"}

// LocalRedirects are the redirects among policies (CiliumLocalRedirectPolicy
// objects) that send the traffic of one of dns to pods of their own
// namespace. Only a policy in kube-system is taken, as ServesDNS takes only a
// Service there. A policy that matches an address rather than a Service, has
// no pod selector or names no usable port is skipped: it widens nothing.
func LocalRedirects(dns []corev1.Service, policies []unstructured.Unstructured) []LocalRedirect {
	served := map[string]bool{}
	for i := range dns {
		if ServesDNS(&dns[i]) {
			served[dns[i].Namespace+"/"+dns[i].Name] = true
		}
	}
	var out []LocalRedirect
	for i := range policies {
		p := &policies[i]
		if p.GetNamespace() != DNSNamespace {
			continue
		}
		name, _, _ := unstructured.NestedString(p.Object, "spec", "redirectFrontend", "serviceMatcher", "serviceName")
		ns, _, _ := unstructured.NestedString(p.Object, "spec", "redirectFrontend", "serviceMatcher", "namespace")
		if ns == "" {
			ns = p.GetNamespace()
		}
		if name == "" || !served[ns+"/"+name] {
			continue
		}
		raw, found, _ := unstructured.NestedMap(p.Object, "spec", "redirectBackend", "localEndpointSelector")
		if !found {
			continue
		}
		var selector metav1.LabelSelector
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &selector); err != nil ||
			(len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0) {
			continue
		}
		ports := backendPorts(p)
		if len(ports) == 0 {
			continue
		}
		out = append(out, LocalRedirect{Namespace: p.GetNamespace(), Selector: selector, Ports: ports})
	}
	return out
}

// backendPorts are a redirect policy's redirectBackend.toPorts as policy
// ports. A port without a protocol, or with ANY, is both UDP and TCP.
func backendPorts(p *unstructured.Unstructured) []networkingv1.NetworkPolicyPort {
	list, _, _ := unstructured.NestedSlice(p.Object, "spec", "redirectBackend", "toPorts")
	var out []networkingv1.NetworkPolicyPort
	for _, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		portStr, _ := m["port"].(string)
		n, err := strconv.ParseInt(portStr, 10, 32)
		if err != nil || n <= 0 || n > 65535 {
			continue
		}
		port := intstr.FromInt32(int32(n))
		protocol, _ := m["protocol"].(string)
		switch strings.ToUpper(protocol) {
		case "UDP":
			out = append(out, networkingv1.NetworkPolicyPort{Protocol: ptrTo(corev1.ProtocolUDP), Port: &port})
		case "TCP":
			out = append(out, networkingv1.NetworkPolicyPort{Protocol: ptrTo(corev1.ProtocolTCP), Port: &port})
		case "", "ANY":
			out = append(out,
				networkingv1.NetworkPolicyPort{Protocol: ptrTo(corev1.ProtocolUDP), Port: &port},
				networkingv1.NetworkPolicyPort{Protocol: ptrTo(corev1.ProtocolTCP), Port: &port})
		}
	}
	return out
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
	for i := range c.DNS.Services {
		if ServesDNS(&c.DNS.Services[i]) {
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
// two rules hold in either order. Where the traffic is redirected to a
// node-local cache first (LocalRedirect), the cache's pods on their ports are
// a third rule: the policy sees neither the Service's address nor its pods.
//
// With no DNS Service found, port 53 is open to every address: a tenant
// whose pods cannot resolve a name cannot run at all, and a policy is not
// what should take a cluster's tenants down. The operator logs the refusal
// to narrow (loadClusterServices in internal/controller).
func DNSEgress(cluster ClusterDNS) []networkingv1.NetworkPolicyEgressRule {
	dns := cluster.Services
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
	if len(out) > 0 {
		for _, r := range cluster.Redirects {
			selector := r.Selector
			out = append(out, networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": r.Namespace,
					}},
					PodSelector: &selector,
				}},
				Ports: r.Ports,
			})
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
