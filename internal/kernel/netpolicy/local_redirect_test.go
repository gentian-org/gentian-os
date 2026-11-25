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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

// nodeLocalDNSRedirect is the redirect policy a Gardener shoot with Cilium
// and node-local-dns carries: traffic for kube-system/kube-dns goes to the
// node-local-dns pod on the sender's node.
func nodeLocalDNSRedirect(namespace, service string, toPorts ...interface{}) unstructured.Unstructured {
	if len(toPorts) == 0 {
		toPorts = []interface{}{
			map[string]interface{}{"name": "dns", "port": "53", "protocol": "UDP"},
			map[string]interface{}{"name": "dns-tcp", "port": "53", "protocol": "TCP"},
		}
	}
	u := unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"redirectFrontend": map[string]interface{}{
				"serviceMatcher": map[string]interface{}{"namespace": "kube-system", "serviceName": service},
			},
			"redirectBackend": map[string]interface{}{
				"localEndpointSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{"k8s-app": "node-local-dns"},
				},
				"toPorts": toPorts,
			},
		},
	}}
	u.SetGroupVersionKind(netpolicy.LocalRedirectPolicyGVK)
	u.SetName("nodelocaldns")
	u.SetNamespace(namespace)
	return u
}

func kubeDNS() corev1.Service {
	return dnsService("kube-dns", map[string]string{"k8s-app": "kube-dns"}, "10.0.0.10",
		servicePort(53, corev1.ProtocolUDP, dns53), servicePort(53, corev1.ProtocolTCP, dns53))
}

// Where Cilium redirects the DNS Service to a node-local cache, the policy
// sees the cache's pod, so DNSEgress admits it beside the Service's own pods
// and address.
func TestDNSEgressAdmitsTheNodeLocalCacheARedirectSendsDNSTo(t *testing.T) {
	dns := []corev1.Service{kubeDNS()}
	redirects := netpolicy.LocalRedirects(dns, []unstructured.Unstructured{nodeLocalDNSRedirect("kube-system", "kube-dns")})
	if len(redirects) != 1 {
		t.Fatalf("redirects = %+v, want the one for kube-dns", redirects)
	}
	rules := netpolicy.DNSEgress(netpolicy.ClusterDNS{Services: dns, Redirects: redirects})
	everyRuleNamesAPeer(t, rules)
	cache := pods("kube-system", map[string]string{"k8s-app": "node-local-dns"})
	if !admits(rules, corev1.ProtocolUDP, dns53, cache) || !admits(rules, corev1.ProtocolTCP, dns53, cache) {
		t.Errorf("the node-local cache is not admitted on 53: %+v", rules)
	}
	if !admits(rules, corev1.ProtocolUDP, dns53, pods("kube-system", map[string]string{"k8s-app": "kube-dns"})) ||
		!admits(rules, corev1.ProtocolUDP, dns53, block("10.0.0.10/32")) {
		t.Errorf("the Service's own pods and address are no longer admitted: %+v", rules)
	}
}

// A redirect widens a policy only for the cluster's DNS: one in another
// namespace, one for another Service, or one without a pod selector or a
// usable port is not taken.
func TestLocalRedirectsTakesOnlyARedirectOfTheClustersDNS(t *testing.T) {
	dns := []corev1.Service{kubeDNS()}
	noSelector := nodeLocalDNSRedirect("kube-system", "kube-dns")
	unstructured.RemoveNestedField(noSelector.Object, "spec", "redirectBackend", "localEndpointSelector")
	for name, policy := range map[string]unstructured.Unstructured{
		"another namespace": nodeLocalDNSRedirect("tenant-acme", "kube-dns"),
		"another Service":   nodeLocalDNSRedirect("kube-system", "metrics-server"),
		"no pod selector":   noSelector,
		"no usable port":    nodeLocalDNSRedirect("kube-system", "kube-dns", map[string]interface{}{"port": "dns"}),
	} {
		if got := netpolicy.LocalRedirects(dns, []unstructured.Unstructured{policy}); len(got) != 0 {
			t.Errorf("%s: redirects = %+v, want none", name, got)
		}
	}
	if got := netpolicy.LocalRedirects(nil, []unstructured.Unstructured{nodeLocalDNSRedirect("kube-system", "kube-dns")}); len(got) != 0 {
		t.Errorf("with no DNS Service found: redirects = %+v, want none", got)
	}
}

// A port with no protocol, or ANY, is admitted on UDP and TCP both.
func TestLocalRedirectsReadsAPortWithoutProtocolAsBoth(t *testing.T) {
	dns := []corev1.Service{kubeDNS()}
	got := netpolicy.LocalRedirects(dns, []unstructured.Unstructured{
		nodeLocalDNSRedirect("kube-system", "kube-dns", map[string]interface{}{"port": "53"}),
	})
	if len(got) != 1 || len(got[0].Ports) != 2 {
		t.Fatalf("redirects = %+v, want one on 53/UDP and 53/TCP", got)
	}
}
