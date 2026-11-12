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
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func modelGatewayDatabaseEgressClient(t *testing.T, llm string, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs = append(objs,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: llmNamespace}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: clusterConfigName, Namespace: clusterConfigNamespace},
			Data:       map[string]string{clusterConfigLLMKey: llm},
		})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func kubeAPIEndpointSlice(port int32, addresses ...string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubernetes", Namespace: metav1.NamespaceDefault,
			Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: addresses}},
		Ports:       []discoveryv1.EndpointPort{{Name: ptr("https"), Protocol: ptr(corev1.ProtocolTCP), Port: ptr(port)}},
	}
}

func reconcileModelGatewayDatabaseEgress(t *testing.T, c client.Client, on bool) *networkingv1.NetworkPolicy {
	t.Helper()
	r := &ModelGatewayDatabaseEgressReconciler{Client: c, NetworkPolicies: on, KubeAPIServerCIDR: "10.0.0.0/8"}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	policy := &networkingv1.NetworkPolicy{}
	err := c.Get(context.Background(), types.NamespacedName{Name: "llm-database-egress", Namespace: llmNamespace}, policy)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// egressAdmits reports whether a rule admits TCP or UDP to port at the peer
// described by match.
func egressAdmits(policy *networkingv1.NetworkPolicy, protocol corev1.Protocol, port int32, match func(networkingv1.NetworkPolicyPeer) bool) bool {
	for _, rule := range policy.Spec.Egress {
		portOK := false
		for _, p := range rule.Ports {
			if p.Protocol != nil && *p.Protocol == protocol && p.Port != nil && p.Port.IntVal == port {
				portOK = true
			}
		}
		if !portOK {
			continue
		}
		if len(rule.To) == 0 && match == nil {
			return true
		}
		for _, peer := range rule.To {
			if match != nil && match(peer) {
				return true
			}
		}
	}
	return false
}

func ipBlock(cidr string) func(networkingv1.NetworkPolicyPeer) bool {
	return func(p networkingv1.NetworkPolicyPeer) bool {
		return p.IPBlock != nil && p.IPBlock.CIDR == cidr && len(p.IPBlock.Except) == 0
	}
}

// The policy selects the database's instances and restricts their egress
// alone; it admits DNS, the API server and the cluster's own pods on the data
// port, and nothing else. Argo CD's tracking annotation is not on it.
func TestTheGatewayDatabaseEgressPolicyAdmitsDNSTheAPIServerAndItsPeers(t *testing.T) {
	c := modelGatewayDatabaseEgressClient(t, "true")
	policy := reconcileModelGatewayDatabaseEgress(t, c, true)
	if policy == nil {
		t.Fatal("no llm-database-egress while the cluster serves models and the switch is on")
	}
	want := map[string]string{"cnpg.io/cluster": "litellm-db", "cnpg.io/podRole": "instance"}
	if !equalStringMaps(policy.Spec.PodSelector.MatchLabels, want) || len(policy.Spec.PodSelector.MatchExpressions) != 0 {
		t.Fatalf("podSelector = %+v, want %v", policy.Spec.PodSelector, want)
	}
	if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(policy.Spec.Ingress) != 0 {
		t.Fatalf("policyTypes = %v, ingress = %v: it restricts egress alone", policy.Spec.PolicyTypes, policy.Spec.Ingress)
	}
	if policy.Labels[managedByLabel] != managedByValue {
		t.Fatalf("labels = %v: not marked as the operator's", policy.Labels)
	}
	if _, tracked := policy.Annotations["argocd.argoproj.io/tracking-id"]; tracked {
		t.Fatal("the policy carries Argo CD's tracking annotation: the gateway's Application would prune it")
	}

	if !egressAdmits(policy, corev1.ProtocolUDP, 53, nil) || !egressAdmits(policy, corev1.ProtocolTCP, 53, nil) {
		t.Fatal("DNS is not admitted")
	}
	if !egressAdmits(policy, corev1.ProtocolTCP, 443, ipBlock("10.0.0.0/8")) {
		t.Fatal("KUBE_APISERVER_CIDR on 443 is not admitted")
	}
	peers := func(p networkingv1.NetworkPolicyPeer) bool {
		return p.NamespaceSelector == nil && p.IPBlock == nil && p.PodSelector != nil &&
			equalStringMaps(p.PodSelector.MatchLabels, map[string]string{"cnpg.io/cluster": "litellm-db"})
	}
	if !egressAdmits(policy, corev1.ProtocolTCP, 5432, peers) {
		t.Fatal("the cluster's own pods are not admitted on 5432")
	}
	// DNS, the API server's range, the peers: nothing else without an
	// EndpointSlice.
	if len(policy.Spec.Egress) != 3 {
		t.Fatalf("egress = %+v, want DNS, the API server and the peers only", policy.Spec.Egress)
	}
	for _, rule := range policy.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == 8000 {
				t.Fatal("the status port is admitted: no instance asks another's")
			}
		}
	}
}

// The API server's addresses come from the kubernetes Service's EndpointSlice,
// and a change of them reaches the policy.
func TestTheGatewayDatabaseEgressPolicyFollowsTheAPIServersEndpointSlice(t *testing.T) {
	slice := kubeAPIEndpointSlice(6443, "203.0.113.10", "203.0.113.11")
	c := modelGatewayDatabaseEgressClient(t, "true", slice)
	policy := reconcileModelGatewayDatabaseEgress(t, c, true)
	for _, addr := range []string{"203.0.113.10/32", "203.0.113.11/32"} {
		if !egressAdmits(policy, corev1.ProtocolTCP, 6443, ipBlock(addr)) {
			t.Fatalf("%s on 6443 is not admitted: %+v", addr, policy.Spec.Egress)
		}
	}

	moved := &discoveryv1.EndpointSlice{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(slice), moved); err != nil {
		t.Fatal(err)
	}
	moved.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"198.51.100.7"}}}
	if err := c.Update(context.Background(), moved); err != nil {
		t.Fatal(err)
	}
	policy = reconcileModelGatewayDatabaseEgress(t, c, true)
	if !egressAdmits(policy, corev1.ProtocolTCP, 6443, ipBlock("198.51.100.7/32")) {
		t.Fatalf("the new address is not admitted: %+v", policy.Spec.Egress)
	}
	if egressAdmits(policy, corev1.ProtocolTCP, 6443, ipBlock("203.0.113.10/32")) {
		t.Fatal("an address the API server no longer answers on is still admitted")
	}
}

// Off -- the switch, or a cluster that serves no models -- the policy is
// removed; a namespace that is not there is no error.
func TestTheGatewayDatabaseEgressPolicyIsRemovedWhenOff(t *testing.T) {
	c := modelGatewayDatabaseEgressClient(t, "true")
	if reconcileModelGatewayDatabaseEgress(t, c, true) == nil {
		t.Fatal("no policy to begin with")
	}
	if reconcileModelGatewayDatabaseEgress(t, c, false) != nil {
		t.Fatal("STORE_NETWORK_POLICIES=false left the policy in place")
	}
	// Twice: nothing there to remove is no error.
	if reconcileModelGatewayDatabaseEgress(t, c, false) != nil {
		t.Fatal("the policy came back with the switch off")
	}

	c = modelGatewayDatabaseEgressClient(t, "false")
	if err := c.Create(context.Background(), modelGatewayDatabaseEgressPolicy("10.0.0.0/8", nil)); err != nil {
		t.Fatal(err)
	}
	if reconcileModelGatewayDatabaseEgress(t, c, true) != nil {
		t.Fatal("a cluster that serves no models kept the policy")
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	empty := fake.NewClientBuilder().WithScheme(scheme).Build()
	if reconcileModelGatewayDatabaseEgress(t, empty, true) != nil {
		t.Fatal("a policy was written without the gateway's namespace")
	}
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
