/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The edge as Envoy Gateway makes it: one LoadBalancer Service for the
// GatewayClass, its public address, 443 and 80 forwarded to the pods'
// 10443 and 10080.
func edgeService(ip string) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "envoy-gentian", Namespace: "kernel-edge",
			Labels: map[string]string{"gateway.envoyproxy.io/owning-gatewayclass": GentianGatewayClassName},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{"app.kubernetes.io/name": "envoy", "gateway.envoyproxy.io/owning-gatewayclass": GentianGatewayClassName},
			Ports: []corev1.ServicePort{
				{Name: "https", Port: 443, TargetPort: intstr.FromInt32(10443), Protocol: corev1.ProtocolTCP},
				{Name: "http", Port: 80, TargetPort: intstr.FromInt32(10080), Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if ip != "" {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
	}
	return svc
}

// The rule a profile asks for to reach public addresses: the public address
// space on one port, the private ranges excepted.
func publicOn(ports ...int32) networkingv1.NetworkPolicyEgressRule {
	rule := networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{
			CIDR:   "0.0.0.0/0",
			Except: []string{"10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16"},
		}}},
	}
	for _, p := range ports {
		tcp := corev1.ProtocolTCP
		port := intstr.FromInt32(p)
		rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	return rule
}

func ports(rule *networkingv1.NetworkPolicyEgressRule) []int {
	var out []int
	for _, p := range rule.Ports {
		out = append(out, p.Port.IntValue())
	}
	return out
}

// A rule to public addresses on 443 reaches the cluster's own public address
// on 443, which from inside is the edge's pods on 10443: that, by the
// Service's own selector in its namespace, and not 10080.
func TestPublicAddressesOn443ReachTheEdgesPodsOnTheirPort(t *testing.T) {
	rule := ownPublicAddressEgress(edgeService("179.237.116.12"), []networkingv1.NetworkPolicyEgressRule{publicOn(443)})
	if rule == nil {
		t.Fatal("no rule for the cluster's own address")
	}
	if got := ports(rule); len(got) != 1 || got[0] != 10443 {
		t.Fatalf("ports = %v, want [10443]", got)
	}
	peer := rule.To[0]
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kernel-edge" ||
		peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "envoy" || peer.IPBlock != nil {
		t.Fatalf("peer = %+v", peer)
	}

	// No ports is every port: both of the Service's.
	all := ownPublicAddressEgress(edgeService("179.237.116.12"), []networkingv1.NetworkPolicyEgressRule{publicOn()})
	if got := ports(all); len(got) != 2 || got[0] != 10443 || got[1] != 10080 {
		t.Fatalf("ports for a rule without ports = %v", got)
	}
}

// Nothing is added where the rules do not admit the address: another port, a
// block without it, an address the block excepts, a rule that names a
// namespace rather than addresses -- nor where the edge has no public address.
func TestNothingIsOpenedWhereTheRulesDoNotAdmitTheOwnAddress(t *testing.T) {
	narrow := publicOn(443)
	narrow.To[0].IPBlock = &networkingv1.IPBlock{CIDR: "203.0.113.0/24"}
	namespaced := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kernel-control"}},
	}}}
	for name, c := range map[string]struct {
		svc   *corev1.Service
		rules []networkingv1.NetworkPolicyEgressRule
	}{
		"another port":            {edgeService("179.237.116.12"), []networkingv1.NetworkPolicyEgressRule{publicOn(5432)}},
		"a block without it":      {edgeService("179.237.116.12"), []networkingv1.NetworkPolicyEgressRule{narrow}},
		"an excepted address":     {edgeService("10.0.0.5"), []networkingv1.NetworkPolicyEgressRule{publicOn(443)}},
		"a namespace, no address": {edgeService("179.237.116.12"), []networkingv1.NetworkPolicyEgressRule{namespaced}},
		"no public address":       {edgeService(""), []networkingv1.NetworkPolicyEgressRule{publicOn(443)}},
		"no rules":                {edgeService("179.237.116.12"), nil},
	} {
		if rule := ownPublicAddressEgress(c.svc, c.rules); rule != nil {
			t.Errorf("%s: opened %+v", name, rule)
		}
	}
}

// Through the reconciler: a component granted public addresses on 443 gets
// the edge's pods on 10443 in its policy, beside the granted rule itself.
func TestAGrantedComponentsPolicyReachesTheClustersOwnAddress(t *testing.T) {
	ctx := context.Background()
	scheme := appStoreScheme(t)
	tenant := acmeTenantFixture()
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	granted := publicOn(443)
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
		Egress: []gentianov1alpha1.EgressRequest{{Name: "intake", Reason: "reads the website's order requests", Rule: granted}},
	}}
	comp := tileFixtureComponent("shop", "tenant-acme", "shop")
	comp.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{{
		Privilege: "egress/intake", Approver: "u-1", ApprovedAt: metav1.Now(), Reason: "approved for the test",
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp.DeepCopy(), edgeService("179.237.116.12")).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelRealm: "kernel", KernelDomain: "k.example"}
	live := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(comp), live); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, live, profile, tenant, false); err != nil {
		t.Fatal(err)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: "component-shop", Namespace: "tenant-acme"}, np); err != nil {
		t.Fatal(err)
	}
	var sawGranted, sawEdge bool
	for i := range np.Spec.Egress {
		rule := &np.Spec.Egress[i]
		if len(rule.To) == 1 && rule.To[0].IPBlock != nil && rule.To[0].IPBlock.CIDR == "0.0.0.0/0" {
			sawGranted = true
		}
		if len(rule.To) == 1 && rule.To[0].PodSelector != nil &&
			rule.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] == "envoy" {
			if got := ports(rule); len(got) == 1 && got[0] == 10443 {
				sawEdge = true
			}
		}
	}
	if !sawGranted || !sawEdge {
		t.Fatalf("granted rule %v, own address %v, in %+v", sawGranted, sawEdge, np.Spec.Egress)
	}
}

// The same for an app the Composition installs, whose granted rules are a
// policy of their own: the edge's pods are in it, beside the granted rule.
func TestAGrantedCompositionAppsPolicyReachesTheClustersOwnAddress(t *testing.T) {
	ctx := context.Background()
	scheme := appStoreScheme(t)
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
		Egress: []gentianov1alpha1.EgressRequest{{Name: "intake", Reason: "reads the website's order requests", Rule: publicOn(443)}},
	}}
	comp := tileFixtureComponent("shop", "tenant-acme", "shop")
	comp.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{{
		Privilege: "egress/intake", Approver: "u-1", ApprovedAt: metav1.Now(), Reason: "approved for the test",
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(comp.DeepCopy(), edgeService("179.237.116.12")).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	live := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(comp), live); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureGrantedEgress(ctx, live, profile); err != nil {
		t.Fatal(err)
	}
	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: grantedEgressPolicyName(live), Namespace: "tenant-acme"}, np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Egress) != 2 || np.Spec.Egress[1].To[0].PodSelector == nil {
		t.Fatalf("egress = %+v", np.Spec.Egress)
	}
	if got := ports(&np.Spec.Egress[1]); len(got) != 1 || got[0] != 10443 {
		t.Fatalf("own address ports = %v", got)
	}
}
