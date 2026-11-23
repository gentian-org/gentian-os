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
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// The cluster's DNS Service as the Aluvian cluster has it: a name of the
// distribution's own, CoreDNS labeled k8s-app=coredns.
func clusterDNSService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pck-k9pfly2-addon-coredns", Namespace: "kube-system"},
		Spec: corev1.ServiceSpec{
			Selector:   map[string]string{"k8s-app": "coredns", "app.kubernetes.io/name": "coredns"},
			ClusterIP:  "10.96.0.10",
			ClusterIPs: []string{"10.96.0.10"},
			Ports: []corev1.ServicePort{
				{Name: "udp-53", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(53)},
				{Name: "tcp-53", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(53)},
			},
		},
	}
}

func kubernetesService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.96.0.1", ClusterIPs: []string{"10.96.0.1"},
			Ports: []corev1.ServicePort{{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP}},
		},
	}
}

func clusterServicesClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// The DNS is read from kube-system's Services that serve 53 and select pods,
// the API server from default's kubernetes Service and its EndpointSlice. A
// Service on 53 or a "kubernetes" EndpointSlice in another namespace is no
// part of it: a tenant could name one so.
func TestLoadClusterServicesReadsTheDNSAndTheAPIServerWhereTheyAre(t *testing.T) {
	metrics := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "metrics-server", Namespace: "kube-system"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"k8s-app": "metrics-server"},
			Ports: []corev1.ServicePort{{Port: 443}}},
	}
	impostor := clusterDNSService()
	impostor.Name, impostor.Namespace = "resolver", "tenant-acme"
	foreignSlice := kubeAPIEndpointSlice(443, "198.51.100.66")
	foreignSlice.Name, foreignSlice.Namespace = "kubernetes-foreign", "tenant-acme"

	c := clusterServicesClient(t, clusterDNSService(), metrics, impostor, kubernetesService(),
		kubeAPIEndpointSlice(31099, "83.166.143.133"), foreignSlice)
	got := loadClusterServices(context.Background(), c)

	if len(got.DNS) != 1 || got.DNS[0].Name != "pck-k9pfly2-addon-coredns" {
		t.Fatalf("DNS = %+v, want the kube-system DNS Service alone", got.DNS)
	}
	if got.APIService == nil || got.APIService.Spec.ClusterIP != "10.96.0.1" {
		t.Fatalf("APIService = %+v", got.APIService)
	}
	if len(got.APIEndpoints) != 1 || got.APIEndpoints[0].Namespace != "default" {
		t.Fatalf("APIEndpoints = %+v, want default's slice alone", got.APIEndpoints)
	}

	empty := loadClusterServices(context.Background(), clusterServicesClient(t))
	if empty.HasDNS() || empty.HasAPIEndpoints() || empty.APIService != nil {
		t.Fatalf("an empty cluster read as %+v", empty)
	}
}

// The tenants, the model gateway's database and the runs are woken by a DNS
// Service and the API server's Service and EndpointSlice: their arrival,
// their going, and a change of what a policy is made of. Nothing else of a
// Service wakes them.
func TestClusterServiceChangesWakeThePoliciesThatAdmitThem(t *testing.T) {
	both, dnsOnly := clusterServiceChanged(), clusterDNSChanged()

	dns := clusterDNSService()
	if !both.Create(event.CreateEvent{Object: dns}) || !dnsOnly.Create(event.CreateEvent{Object: dns}) {
		t.Error("a DNS Service that arrives wakes nobody")
	}
	if !both.Delete(event.DeleteEvent{Object: dns}) || !dnsOnly.Delete(event.DeleteEvent{Object: dns}) {
		t.Error("a DNS Service that goes wakes nobody")
	}

	reselected := dns.DeepCopy()
	reselected.Spec.Selector = map[string]string{"k8s-app": "kube-dns"}
	readdressed := dns.DeepCopy()
	readdressed.Spec.ClusterIPs = []string{"10.96.0.20"}
	stopped := dns.DeepCopy()
	stopped.Spec.Ports = []corev1.ServicePort{{Port: 9153}}
	for name, updated := range map[string]*corev1.Service{
		"selector": reselected, "cluster address": readdressed, "no longer on 53": stopped,
	} {
		if !both.Update(event.UpdateEvent{ObjectOld: dns, ObjectNew: updated}) ||
			!dnsOnly.Update(event.UpdateEvent{ObjectOld: dns, ObjectNew: updated}) {
			t.Errorf("a DNS Service whose %s changed wakes nobody", name)
		}
	}
	started := stopped.DeepCopy()
	started.Spec.Ports = dns.Spec.Ports
	if !dnsOnly.Update(event.UpdateEvent{ObjectOld: stopped, ObjectNew: started}) {
		t.Error("a Service that starts serving 53 wakes nobody")
	}
	annotated := dns.DeepCopy()
	annotated.Annotations = map[string]string{"x": "y"}
	if both.Update(event.UpdateEvent{ObjectOld: dns, ObjectNew: annotated}) {
		t.Error("an annotation of the DNS Service wakes every tenant")
	}

	other := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "tenant-acme"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "web"}, Ports: []corev1.ServicePort{{Port: 53}}}}
	if both.Create(event.CreateEvent{Object: other}) || dnsOnly.Create(event.CreateEvent{Object: other}) {
		t.Error("a tenant's Service on 53 wakes the cluster's policies")
	}

	api := kubernetesService()
	if !both.Create(event.CreateEvent{Object: api}) {
		t.Error("the kubernetes Service wakes nobody")
	}
	if dnsOnly.Create(event.CreateEvent{Object: api}) {
		t.Error("the kubernetes Service wakes what admits the DNS alone")
	}
	slice := kubeAPIEndpointSlice(6443, "203.0.113.10")
	moved := slice.DeepCopy()
	moved.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"198.51.100.7"}}}
	if !both.Update(event.UpdateEvent{ObjectOld: slice, ObjectNew: moved}) {
		t.Error("the API server's addresses changed and nobody is woken")
	}
	relabeled := slice.DeepCopy()
	relabeled.Annotations = map[string]string{"x": "y"}
	if both.Update(event.UpdateEvent{ObjectOld: slice, ObjectNew: relabeled}) {
		t.Error("an annotation of the API server's EndpointSlice wakes every tenant")
	}
	foreign := slice.DeepCopy()
	foreign.Namespace = "tenant-acme"
	if both.Create(event.CreateEvent{Object: foreign}) {
		t.Error("a kubernetes EndpointSlice outside default wakes the cluster's policies")
	}
}

// The DNS a perimeter proxy may ask reaches every component that has one,
// found by its proxy's egress policy in the tenant's DMZ.
func TestADNSChangeWakesTheComponentsWithAPerimeter(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	proxy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: "web-public-egress", Namespace: layout.TenantDMZ("acme"),
		Labels: map[string]string{"app.kubernetes.io/name": perimeterProxyAppName, "gentianos.io/component": "web"},
	}}
	unrelated := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant-isolation", Namespace: tenant.NamespaceName(),
	}}
	c := clusterServicesClient(t, tenant, proxy, unrelated)

	q := &recordingQueue{}
	componentsWithPerimeter(c).Create(context.Background(), event.CreateEvent{Object: clusterDNSService()}, q)
	want := types.NamespacedName{Name: "web", Namespace: tenant.NamespaceName()}
	if len(q.added) != 1 || q.added[0].NamespacedName != want {
		t.Fatalf("woken = %v, want %v", q.added, want)
	}
}

// The model gateway's database asks the cluster's DNS and the API server by
// their own addresses once they are read: no rule is open to every address,
// and the wide range is gone.
func TestTheGatewayDatabaseEgressPolicyNamesTheClustersDNSAndAPIServer(t *testing.T) {
	c := modelGatewayDatabaseEgressClient(t, "true", clusterDNSService(), kubernetesService(),
		kubeAPIEndpointSlice(31099, "83.166.143.133"))
	policy := reconcileModelGatewayDatabaseEgress(t, c, true)
	for _, rule := range policy.Spec.Egress {
		if len(rule.To) == 0 {
			t.Fatalf("a rule admits every address: %+v", rule)
		}
	}
	if egressAdmits(policy, corev1.ProtocolTCP, 443, ipBlock("10.0.0.0/8")) {
		t.Fatal("the wide range is admitted")
	}
	coreDNS := func(p networkingv1.NetworkPolicyPeer) bool {
		return p.PodSelector != nil && p.NamespaceSelector != nil &&
			p.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "kube-system" &&
			p.PodSelector.MatchLabels["k8s-app"] == "coredns"
	}
	for _, protocol := range []corev1.Protocol{corev1.ProtocolUDP, corev1.ProtocolTCP} {
		if !egressAdmits(policy, protocol, 53, coreDNS) || !egressAdmits(policy, protocol, 53, ipBlock("10.96.0.10/32")) {
			t.Fatalf("DNS over %s is not the cluster's: %+v", protocol, policy.Spec.Egress)
		}
	}
	if !egressAdmits(policy, corev1.ProtocolTCP, 31099, ipBlock("83.166.143.133/32")) ||
		!egressAdmits(policy, corev1.ProtocolTCP, 443, ipBlock("10.96.0.1/32")) {
		t.Fatalf("the API server is not admitted by its addresses: %+v", policy.Spec.Egress)
	}
}

// A run asks the cluster's DNS and no other resolver.
func TestARunsDNSIsTheClusters(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	runs := &tenantRuns{tenant: tenant}
	policies := runNamespacePolicies(runs, layout.TenantRuns("acme"), tenant.NamespaceName(), nil,
		[]corev1.Service{*clusterDNSService()}, time.Now())
	deny := policies[0]
	if deny.Name != runsDefaultDeny || len(deny.Spec.Egress) == 0 {
		t.Fatalf("default = %+v", deny)
	}
	for _, rule := range deny.Spec.Egress {
		if len(rule.To) == 0 {
			t.Fatalf("a run may ask any resolver: %+v", rule)
		}
	}
}
