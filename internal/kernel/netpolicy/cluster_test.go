/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

func dnsService(name string, selector map[string]string, clusterIP string, ports ...corev1.ServicePort) corev1.Service {
	return corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Spec: corev1.ServiceSpec{
			Selector: selector, ClusterIP: clusterIP, ClusterIPs: []string{clusterIP}, Ports: ports,
		},
	}
}

func servicePort(port int32, protocol corev1.Protocol, target intstr.IntOrString) corev1.ServicePort {
	return corev1.ServicePort{Port: port, Protocol: protocol, TargetPort: target}
}

func apiService(clusterIP string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			ClusterIP: clusterIP, ClusterIPs: []string{clusterIP},
			Ports: []corev1.ServicePort{{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP}},
		},
	}
}

func apiSlice(port int32, addresses ...string) discoveryv1.EndpointSlice {
	tcp := corev1.ProtocolTCP
	return discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default",
			Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: addresses}},
		Ports:     []discoveryv1.EndpointPort{{Protocol: &tcp, Port: &port}},
	}
}

// admits reports whether a rule admits protocol/port at a peer match
// accepts; a nil match asks for a rule open to every address.
func admits(rules []networkingv1.NetworkPolicyEgressRule, protocol corev1.Protocol, port intstr.IntOrString,
	match func(networkingv1.NetworkPolicyPeer) bool) bool {
	for _, rule := range rules {
		portOK := false
		for _, p := range rule.Ports {
			if p.Protocol != nil && *p.Protocol == protocol && p.Port != nil && *p.Port == port {
				portOK = true
			}
		}
		if !portOK {
			continue
		}
		if match == nil {
			if len(rule.To) == 0 {
				return true
			}
			continue
		}
		for _, peer := range rule.To {
			if match(peer) {
				return true
			}
		}
	}
	return false
}

func block(cidr string) func(networkingv1.NetworkPolicyPeer) bool {
	return func(p networkingv1.NetworkPolicyPeer) bool {
		return p.IPBlock != nil && p.IPBlock.CIDR == cidr && len(p.IPBlock.Except) == 0 &&
			p.PodSelector == nil && p.NamespaceSelector == nil
	}
}

func pods(namespace string, selector map[string]string) func(networkingv1.NetworkPolicyPeer) bool {
	return func(p networkingv1.NetworkPolicyPeer) bool {
		if p.IPBlock != nil || p.NamespaceSelector == nil || p.PodSelector == nil ||
			len(p.NamespaceSelector.MatchLabels) != 1 ||
			p.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != namespace ||
			len(p.PodSelector.MatchLabels) != len(selector) {
			return false
		}
		for k, v := range selector {
			if p.PodSelector.MatchLabels[k] != v {
				return false
			}
		}
		return true
	}
}

// Every rule names a peer: nothing is open to every address on any port.
func everyRuleNamesAPeer(t *testing.T, rules []networkingv1.NetworkPolicyEgressRule) {
	t.Helper()
	for _, rule := range rules {
		if len(rule.To) == 0 {
			t.Fatalf("a rule admits every address: %+v", rule)
		}
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "10.0.0.0/8" {
				t.Fatalf("the wide range is admitted: %+v", rule)
			}
		}
	}
}

var (
	dns53    = intstr.FromInt32(53)
	https443 = intstr.FromInt32(443)
)

// A cluster's DNS is its DNS Service's pods, by the Service's own selector in
// kube-system, and the Service's cluster address, on 53 over UDP and TCP.
// Ports of the Service other than 53 are not admitted. The facts are the
// Aluvian cluster's (CoreDNS labeled k8s-app=coredns behind a Service of the
// distribution's own name).
func TestDNSEgressIsTheDNSServicesPodsAndClusterAddress(t *testing.T) {
	selector := map[string]string{
		"app.kubernetes.io/instance": "pck-k9pfly2-addon-coredns",
		"app.kubernetes.io/name":     "coredns",
		"k8s-app":                    "coredns",
	}
	svc := dnsService("pck-k9pfly2-addon-coredns", selector, "10.96.0.10",
		servicePort(53, corev1.ProtocolUDP, dns53),
		servicePort(53, corev1.ProtocolTCP, dns53),
		servicePort(9153, corev1.ProtocolTCP, intstr.FromInt32(9153)))
	rules := netpolicy.DNSEgress([]corev1.Service{svc})

	everyRuleNamesAPeer(t, rules)
	for _, protocol := range []corev1.Protocol{corev1.ProtocolUDP, corev1.ProtocolTCP} {
		if !admits(rules, protocol, dns53, pods("kube-system", selector)) {
			t.Errorf("the DNS pods are not admitted on %s/53: %+v", protocol, rules)
		}
		if !admits(rules, protocol, dns53, block("10.96.0.10/32")) {
			t.Errorf("the DNS Service's cluster address is not admitted on %s/53: %+v", protocol, rules)
		}
	}
	if admits(rules, corev1.ProtocolTCP, intstr.FromInt32(9153), pods("kube-system", selector)) {
		t.Error("the DNS Service's metrics port is admitted")
	}
}

// A Service that sends 53 to a named or another port is followed there for
// its pods; its cluster address stays on 53. The beefy1 facts: kube-dns,
// k8s-app=kube-dns, 10.152.183.10.
func TestDNSEgressFollowsTheServicesTargetPort(t *testing.T) {
	selector := map[string]string{"k8s-app": "kube-dns"}
	svc := dnsService("kube-dns", selector, "10.152.183.10",
		servicePort(53, corev1.ProtocolUDP, intstr.FromString("dns")),
		servicePort(53, corev1.ProtocolTCP, intstr.FromInt32(1053)))
	rules := netpolicy.DNSEgress([]corev1.Service{svc})

	if !admits(rules, corev1.ProtocolUDP, intstr.FromString("dns"), pods("kube-system", selector)) {
		t.Errorf("the pods are not admitted on the named target port: %+v", rules)
	}
	if !admits(rules, corev1.ProtocolTCP, intstr.FromInt32(1053), pods("kube-system", selector)) {
		t.Errorf("the pods are not admitted on the target port: %+v", rules)
	}
	if !admits(rules, corev1.ProtocolUDP, dns53, block("10.152.183.10/32")) ||
		!admits(rules, corev1.ProtocolTCP, dns53, block("10.152.183.10/32")) {
		t.Errorf("the cluster address is not admitted on 53: %+v", rules)
	}
}

// With no DNS Service found, port 53 stays open to every address: a policy
// that closed DNS would stop every tenant. A Service that serves 53 outside
// kube-system, or selects no pods, is not the cluster's DNS.
func TestDNSEgressFallsBackToOpen53WithoutADNSService(t *testing.T) {
	elsewhere := dnsService("resolver", map[string]string{"app": "resolver"}, "10.0.0.53",
		servicePort(53, corev1.ProtocolUDP, dns53))
	elsewhere.Namespace = "tenant-acme"
	noSelector := dnsService("external-dns", nil, "10.96.0.11", servicePort(53, corev1.ProtocolUDP, dns53))
	headlessOther := dnsService("metrics", map[string]string{"app": "m"}, "None", servicePort(443, corev1.ProtocolTCP, https443))

	for name, dns := range map[string][]corev1.Service{
		"none":        nil,
		"elsewhere":   {elsewhere},
		"no selector": {noSelector},
		"not port 53": {headlessOther},
	} {
		rules := netpolicy.DNSEgress(dns)
		if len(rules) != 1 || len(rules[0].To) != 0 ||
			!admits(rules, corev1.ProtocolUDP, dns53, nil) || !admits(rules, corev1.ProtocolTCP, dns53, nil) {
			t.Errorf("%s: rules = %+v, want 53 over UDP and TCP to every address", name, rules)
		}
		if (netpolicy.ClusterServices{DNS: dns}).HasDNS() {
			t.Errorf("%s: counted as the cluster's DNS", name)
		}
	}
}

// The API server is each address of the kubernetes EndpointSlice on its port,
// and the kubernetes Service's cluster address on the Service's port; the
// wide range is not admitted once an address is read. The facts are the
// Aluvian cluster's: an API server outside the cluster on 31099, the Service
// on 10.96.0.1:443.
func TestAPIServerEgressIsTheEndpointsAndTheClusterAddress(t *testing.T) {
	rules := netpolicy.APIServerEgress("10.0.0.0/8", netpolicy.ClusterServices{
		APIService:   apiService("10.96.0.1"),
		APIEndpoints: []discoveryv1.EndpointSlice{apiSlice(31099, "83.166.143.133")},
	})
	everyRuleNamesAPeer(t, rules)
	if !admits(rules, corev1.ProtocolTCP, intstr.FromInt32(31099), block("83.166.143.133/32")) {
		t.Errorf("the API server's address is not admitted on its port: %+v", rules)
	}
	if !admits(rules, corev1.ProtocolTCP, https443, block("10.96.0.1/32")) {
		t.Errorf("the kubernetes Service's cluster address is not admitted on 443: %+v", rules)
	}
	if len(rules) != 2 {
		t.Errorf("rules = %+v, want the endpoint and the cluster address only", rules)
	}
}

// The fallback range on 443 is admitted only while no address of the API
// server is read: with nothing read, and with the Service alone, whose cluster
// address a datapath that evaluates after translation does not see.
func TestAPIServerEgressFallsBackToTheRangeWithoutAnAddress(t *testing.T) {
	nothing := netpolicy.APIServerEgress("10.0.0.0/8", netpolicy.ClusterServices{})
	if len(nothing) != 1 || !admits(nothing, corev1.ProtocolTCP, https443, block("10.0.0.0/8")) {
		t.Errorf("nothing read: rules = %+v, want the range on 443", nothing)
	}
	serviceOnly := netpolicy.APIServerEgress("10.0.0.0/8", netpolicy.ClusterServices{
		APIService:   apiService("10.152.183.1"),
		APIEndpoints: []discoveryv1.EndpointSlice{apiSlice(16443)},
	})
	if !admits(serviceOnly, corev1.ProtocolTCP, https443, block("10.0.0.0/8")) ||
		!admits(serviceOnly, corev1.ProtocolTCP, https443, block("10.152.183.1/32")) {
		t.Errorf("the Service alone: rules = %+v, want the range and the cluster address", serviceOnly)
	}
	if got := netpolicy.APIServerEgress("", netpolicy.ClusterServices{}); len(got) != 0 {
		t.Errorf("no range configured: rules = %+v, want none", got)
	}
}

// An IPv6 address is a block of one as well.
func TestClusterAddressesOfIPv6AreSingleHostBlocks(t *testing.T) {
	svc := dnsService("kube-dns", map[string]string{"k8s-app": "kube-dns"}, "fd00::a",
		servicePort(53, corev1.ProtocolUDP, dns53))
	rules := netpolicy.DNSEgress([]corev1.Service{svc})
	if !admits(rules, corev1.ProtocolUDP, dns53, block("fd00::a/128")) {
		t.Errorf("rules = %+v, want fd00::a/128", rules)
	}
	api := netpolicy.APIServerEgress("", netpolicy.ClusterServices{
		APIEndpoints: []discoveryv1.EndpointSlice{apiSlice(6443, "2001:db8::1")},
	})
	if !admits(api, corev1.ProtocolTCP, intstr.FromInt32(6443), block("2001:db8::1/128")) {
		t.Errorf("rules = %+v, want 2001:db8::1/128", api)
	}
}

// A tenant's baseline on the beefy1 facts: DNS narrowed to kube-dns, the API
// server at 192.168.0.100:16443 and 10.152.183.1:443, no rule open to every
// address and no wide range.
func TestBaselineEgressNamesThePeersOfTheClustersServices(t *testing.T) {
	selector := map[string]string{"k8s-app": "kube-dns"}
	cluster := netpolicy.ClusterServices{
		DNS: []corev1.Service{dnsService("kube-dns", selector, "10.152.183.10",
			servicePort(53, corev1.ProtocolUDP, dns53), servicePort(53, corev1.ProtocolTCP, dns53))},
		APIService:   apiService("10.152.183.1"),
		APIEndpoints: []discoveryv1.EndpointSlice{apiSlice(16443, "192.168.0.100")},
	}
	policy := netpolicy.BaselineNetworkPolicy("demo", "tenant-demo", netpolicy.DefaultConfig(), cluster)
	everyRuleNamesAPeer(t, policy.Spec.Egress)
	if !admits(policy.Spec.Egress, corev1.ProtocolUDP, dns53, pods("kube-system", selector)) ||
		!admits(policy.Spec.Egress, corev1.ProtocolUDP, dns53, block("10.152.183.10/32")) ||
		!admits(policy.Spec.Egress, corev1.ProtocolTCP, intstr.FromInt32(16443), block("192.168.0.100/32")) ||
		!admits(policy.Spec.Egress, corev1.ProtocolTCP, https443, block("10.152.183.1/32")) {
		t.Errorf("egress = %+v", policy.Spec.Egress)
	}
}
