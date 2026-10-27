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
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// The daemon's configuration: credentials minted with the shared secret,
// every listener on the pod's own address, TURN over TLS only once there is
// a certificate, and one cluster of peers -- the edge's own pods.
func TestTheTURNEdgeConfigRelaysOnlyToItsOwnPods(t *testing.T) {
	read := func(raw []byte) stunnerdConfig {
		t.Helper()
		var conf stunnerdConfig
		if err := json.Unmarshal(raw, &conf); err != nil {
			t.Fatal(err)
		}
		return conf
	}
	raw, err := turnEdgeConfig("turn.k.example", "0123abcd", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	conf := read(raw)
	if conf.Auth.Type != "ephemeral" || conf.Auth.Credentials["secret"] != "0123abcd" || conf.Auth.Realm != "turn.k.example" {
		t.Fatalf("auth = %+v", conf.Auth)
	}
	if len(conf.Listeners) != 2 {
		t.Fatalf("listeners without a certificate = %+v", conf.Listeners)
	}
	for _, l := range conf.Listeners {
		if l.Address != "$STUNNER_ADDR" || len(l.Routes) != 1 || l.Routes[0] != "peers" {
			t.Fatalf("listener %+v", l)
		}
	}
	if len(conf.Clusters) != 1 || conf.Clusters[0].Type != "STRICT_DNS" ||
		len(conf.Clusters[0].Endpoints) != 1 || conf.Clusters[0].Endpoints[0] != "turn-edge-peers.system-turn-dmz.svc.cluster.local" {
		t.Fatalf("clusters = %+v", conf.Clusters)
	}

	raw, err = turnEdgeConfig("turn.k.example", "0123abcd", []byte("CERT"), []byte("KEY"))
	if err != nil {
		t.Fatal(err)
	}
	conf = read(raw)
	if len(conf.Listeners) != 3 {
		t.Fatalf("listeners with a certificate = %+v", conf.Listeners)
	}
	tls := conf.Listeners[2]
	if tls.Protocol != "TURN-TLS" || tls.Port != turnsListenPort ||
		tls.Cert != base64.StdEncoding.EncodeToString([]byte("CERT")) || tls.Key != base64.StdEncoding.EncodeToString([]byte("KEY")) {
		t.Fatalf("tls listener = %+v", tls)
	}

	// The daemon expands $NAME in the whole file: a secret that holds one
	// would be read as something else, so it is refused.
	if _, err := turnEdgeConfig("turn.k.example", "abc$HOME", nil, nil); err == nil {
		t.Fatal("a secret with a $ was written into the configuration")
	}
}

// The policy: the listeners from anywhere, the relay from the edge's own
// pods, and out to nothing but those pods and the resolver.
func TestTheTURNEdgeReachesNothingButItself(t *testing.T) {
	var policy *networkingv1.NetworkPolicy
	var lb *corev1.Service
	for _, obj := range turnEdgeObjects("img", []byte("{}")) {
		switch o := obj.(type) {
		case *networkingv1.NetworkPolicy:
			policy = o
		case *corev1.Service:
			if o.Name == turnEdgeName {
				lb = o
			}
		}
	}
	if policy == nil || lb == nil {
		t.Fatal("no policy or no load balancer")
	}
	own := turnEdgeLabels()
	if len(policy.Spec.Egress) != 2 {
		t.Fatalf("egress = %+v", policy.Spec.Egress)
	}
	toPods := policy.Spec.Egress[0]
	if len(toPods.To) != 1 || toPods.To[0].NamespaceSelector != nil || toPods.To[0].IPBlock != nil ||
		toPods.To[0].PodSelector == nil || toPods.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != own["app.kubernetes.io/name"] {
		t.Fatalf("egress to the edge's pods = %+v", toPods)
	}
	dns := policy.Spec.Egress[1]
	if len(dns.Ports) != 2 || dns.Ports[0].Port.IntValue() != 53 {
		t.Fatalf("egress to DNS = %+v", dns)
	}
	if lb.Spec.Type != corev1.ServiceTypeLoadBalancer || lb.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Fatalf("load balancer = %+v", lb.Spec)
	}
	var ports []string
	for _, p := range lb.Spec.Ports {
		ports = append(ports, string(p.Protocol)+"/"+p.TargetPort.String())
	}
	if strings.Join(ports, " ") != "UDP/3478 TCP/3478 TCP/5349" {
		t.Fatalf("load balancer ports = %v", ports)
	}
}

func TestTURNIsReachableOnlyWithoutATunnel(t *testing.T) {
	for _, tc := range []struct {
		ingress, mode string
		want          bool
	}{
		{"", "static-ip", true},
		{"", "tunnel", false},
		{"", "", false},
		{"none", "tunnel", true},
		{"cf-tunnel", "static-ip", false},
	} {
		t.Setenv("EDGE_INGRESS", tc.ingress)
		t.Setenv("NETWORK_MODE", tc.mode)
		if got := turnReachable(); got != tc.want {
			t.Errorf("EDGE_INGRESS=%q NETWORK_MODE=%q: reachable = %v", tc.ingress, tc.mode, got)
		}
	}
}

// While the engine is wanted and the namespace is there: the secret is the
// vault's, made once, and the edge, its certificate and its address record
// are written. Nothing while it is not wanted, and nothing behind a tunnel.
func TestTheTURNEdgeIsMadeWhileWanted(t *testing.T) {
	ctx := context.Background()
	t.Setenv("EDGE_INGRESS", "")
	t.Setenv("NETWORK_MODE", "static-ip")
	set := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: systemEnginesConfigMap, Namespace: "kernel-control"},
		Data: map[string]string{EngineMariaDB: engineWanted}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: turnEdgeNamespace}}
	c := fake.NewClientBuilder().WithScheme(componentDatabaseScheme(t)).WithObjects(set, ns).Build()
	vault := newSecretVault()
	seeder := secrets.NewSeeder(vault, secrets.NewDeriver("a master password", "a salt")).WithMode(randomMode)
	r := &TURNEdgeReconciler{Client: c, Namespace: "kernel-control", Seeder: seeder, KernelDomain: "k.example", ClusterIssuer: "issuer"}
	deployment := func() error {
		return c.Get(ctx, types.NamespacedName{Name: turnEdgeName, Namespace: turnEdgeNamespace}, &appsv1.Deployment{})
	}
	if _, err := r.Reconcile(ctx, turnEdgeRequest); err != nil {
		t.Fatal(err)
	}
	if deployment() == nil {
		t.Fatal("an engine nobody wants was deployed")
	}

	set.Data[EngineTURN] = engineWanted
	if err := c.Update(ctx, set); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETWORK_MODE", "tunnel")
	if _, err := r.Reconcile(ctx, turnEdgeRequest); err != nil {
		t.Fatal(err)
	}
	if deployment() == nil {
		t.Fatal("deployed behind a tunnel, where nothing reaches it")
	}

	t.Setenv("NETWORK_MODE", "static-ip")
	if _, err := r.Reconcile(ctx, turnEdgeRequest); err != nil {
		t.Fatal(err)
	}
	if err := deployment(); err != nil {
		t.Fatal(err)
	}
	record, _ := vault.Get(ctx, "gentian-os/kernel/turn/edge")
	if len(record[turnSecretProperty]) < 30 {
		t.Fatalf("vault = %v", record)
	}
	conf := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: turnEdgeConfigSecret, Namespace: turnEdgeNamespace}, conf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf.Data[turnEdgeConfigKey]), record[turnSecretProperty]) {
		t.Fatal("the configuration does not carry the vault's secret")
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certManagerCertGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: turnEdgeTLSSecret, Namespace: turnEdgeNamespace}, cert); err != nil {
		t.Fatal(err)
	}
	if names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames"); strings.Join(names, ",") != "turn.k.example" {
		t.Fatalf("certificate names = %v", names)
	}

	// Once the certificate is issued, TURN over TLS is offered.
	tls := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: turnEdgeTLSSecret, Namespace: turnEdgeNamespace},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("CERT"), corev1.TLSPrivateKeyKey: []byte("KEY")},
	}
	if err := c.Create(ctx, tls); err != nil {
		t.Fatal(err)
	}
	// And once the load balancer has an address, the name points at it.
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Name: turnEdgeName, Namespace: turnEdgeNamespace}, svc); err != nil {
		t.Fatal(err)
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}
	if err := c.Status().Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, turnEdgeRequest); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(conf), conf); err != nil {
		t.Fatal(err)
	}
	var parsed stunnerdConfig
	if err := json.Unmarshal(conf.Data[turnEdgeConfigKey], &parsed); err != nil || len(parsed.Listeners) != 3 {
		t.Fatalf("listeners once the certificate is issued = %+v (%v)", parsed.Listeners, err)
	}
	dns := &unstructured.Unstructured{}
	dns.SetGroupVersionKind(dnsEndpointGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: turnEdgeName, Namespace: turnEdgeNamespace}, dns); err != nil {
		t.Fatal(err)
	}
	endpoints, _, _ := unstructured.NestedSlice(dns.Object, "spec", "endpoints")
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %v", endpoints)
	}
	ep := endpoints[0].(map[string]interface{})
	if ep["dnsName"] != "turn.k.example" || ep["recordType"] != "A" {
		t.Fatalf("record = %v", ep)
	}
}

// A component waits for the edge and is told, on a tunnel cluster, why it
// will not come. An addon's declaration is its base's.
func TestAComponentWaitsForTheTURNEdge(t *testing.T) {
	ctx := context.Background()
	t.Setenv("EDGE_INGRESS", "")
	talk := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "talk"}}
	talk.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{TURN: &gentianov1alpha1.TURNRequirement{}}}
	base := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "base"}}
	if got := declaredEngines(talk); len(got) != 1 || got[0] != EngineTURN {
		t.Fatalf("declared = %v", got)
	}
	if engineNamespace(EngineTURN) != "system-turn-dmz" || engineNamespace(EngineMariaDB) != "system-mariadb" {
		t.Fatal("engine namespaces")
	}
	c := fake.NewClientBuilder().WithScheme(componentDatabaseScheme(t)).Build()

	t.Setenv("NETWORK_MODE", "tunnel")
	engine, why, err := systemEngineWaiting(ctx, c, base, talk)
	if err != nil || engine != EngineTURN || why != turnUnreachableReason {
		t.Fatalf("behind a tunnel: %q %q %v", engine, why, err)
	}

	t.Setenv("NETWORK_MODE", "static-ip")
	if engine, why, err = systemEngineWaiting(ctx, c, base, talk); err != nil || engine != EngineTURN || why != "" {
		t.Fatalf("before the edge answers: %q %q %v", engine, why, err)
	}
	replicas := int32(2)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: turnEdgeName, Namespace: turnEdgeNamespace},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: turnEdgeName, Namespace: turnEdgeNamespace}}
	for _, obj := range []client.Object{dep, svc} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 2, ReadyReplicas: 2, AvailableReplicas: 2, UpdatedReplicas: 2}
	if err := c.Status().Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if engine, _, _ = systemEngineWaiting(ctx, c, base, talk); engine != EngineTURN {
		t.Fatal("ready before the load balancer has an address")
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}
	if err := c.Status().Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if engine, _, err = systemEngineWaiting(ctx, c, base, talk); err != nil || engine != "" {
		t.Fatalf("once the edge answers: %q %v", engine, err)
	}
}
