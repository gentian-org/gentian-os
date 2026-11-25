/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

const hairpinCorefile = `.:53 {
    hosts {
          # BEGIN gentian-hairpin
          10.96.42.170 aluvian.io
          10.96.42.170 store-service.aluvian.io
          # END gentian-hairpin
          fallthrough
    }
    forward . 1.1.1.1
}`

// The hosts the hairpin block maps to the edge are the cluster's own; no
// other line of the Corefile names one.
func TestTheHairpinBlockNamesTheHostsTheClusterServesItself(t *testing.T) {
	got := hairpinHosts(hairpinCorefile)
	if len(got) != 2 {
		t.Fatalf("hairpin hosts %v, want aluvian.io and store-service.aluvian.io", got)
	}
	for _, h := range []string{"aluvian.io", "store-service.aluvian.io"} {
		if _, ok := got[h]; !ok {
			t.Errorf("%s missing from %v", h, got)
		}
	}
	if len(hairpinHosts("forward . 1.1.1.1")) != 0 {
		t.Error("a Corefile without the block names hosts")
	}
}

// A granted host the cluster serves itself is not the gateway's: it would
// resolve to the edge's private address, which the gateway may not reach.
func TestAHostTheClusterServesItselfLeavesTheGateway(t *testing.T) {
	own := hairpinHosts(hairpinCorefile)
	outside, inside := splitOwnHosts([]string{"api.stripe.com", "store-service.aluvian.io", "mcp.plaud.ai:8443"}, own)
	if !slices.Equal(outside, []string{"api.stripe.com", "mcp.plaud.ai:8443"}) {
		t.Errorf("carried by the gateway: %v", outside)
	}
	if !slices.Equal(inside, []string{"store-service.aluvian.io"}) {
		t.Errorf("served by the cluster: %v", inside)
	}
}

// The component reaches a host the cluster serves itself at the edge's
// pods, on the port the host's port forwards to, and on no other.
func TestAHostTheClusterServesItselfOpensTheEdgesPodsOnItsPort(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kernel-edge", Name: "edge"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "edge"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(10080)},
				{Name: "https", Port: 443, TargetPort: intstr.FromInt32(10443)},
			},
		},
	}
	rule := ownHostsEgress(svc, []string{"store-service.aluvian.io"})
	if rule == nil {
		t.Fatal("no rule for a host the cluster serves itself")
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntVal != 10443 {
		t.Fatalf("ports %v, want 10443 alone", rule.Ports)
	}
	peer := rule.To[0]
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kernel-edge" || peer.PodSelector.MatchLabels["app"] != "edge" {
		t.Fatalf("peer %+v, want the edge Service's pods", peer)
	}
	if ownHostsEgress(svc, []string{"store-service.aluvian.io:8443"}) != nil {
		t.Error("a port the edge does not serve opened something")
	}
	if ownHostsEgress(svc, nil) != nil {
		t.Error("no host opened something")
	}
}

// A change to the hairpin hosts wakes the apps asking for egress; any other
// change to CoreDNS's ConfigMap, or a ConfigMap elsewhere, does not.
func TestAChangeToTheHairpinHostsWakesTheAppsAskingForEgress(t *testing.T) {
	cm := func(ns, corefile string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "coredns"}, Data: map[string]string{"Corefile": corefile}}
	}
	changed := hairpinChanged()
	fewer := `# BEGIN gentian-hairpin
10.96.42.170 aluvian.io
# END gentian-hairpin`
	if !changed.Update(event.UpdateEvent{ObjectOld: cm("kube-system", hairpinCorefile), ObjectNew: cm("kube-system", fewer)}) {
		t.Error("a host leaving the hairpin woke nothing")
	}
	if changed.Update(event.UpdateEvent{ObjectOld: cm("kube-system", hairpinCorefile), ObjectNew: cm("kube-system", hairpinCorefile+"\n# other")}) {
		t.Error("a change outside the hairpin woke the apps")
	}
	if changed.Update(event.UpdateEvent{ObjectOld: cm("tenant-acme", hairpinCorefile), ObjectNew: cm("tenant-acme", fewer)}) {
		t.Error("a ConfigMap outside kube-system woke the apps")
	}
}
