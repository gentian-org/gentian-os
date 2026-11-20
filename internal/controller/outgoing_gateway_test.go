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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/dispatcher"
	"github.com/gentian-org/gentian-os/internal/egress"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/layout"
)

func outgoingScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	return scheme
}

func componentWithHosts(namespace, name string, hosts ...string) *gentianov1alpha1.Component {
	return &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status:     gentianov1alpha1.ComponentStatus{OutgoingHosts: hosts},
	}
}

func outgoingClass() *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: netpolicy.OutgoingGatewayClassName}}
}

type outgoingState struct {
	listeners map[string]int32 // host name -> listener port
	services  map[string]*corev1.Service
	policies  map[string]*networkingv1.NetworkPolicy
	routes    int
}

func readOutgoing(t *testing.T, c client.Client) outgoingState {
	t.Helper()
	ctx := context.Background()
	edge := client.InNamespace(layout.Namespace(layout.Edge))
	out := outgoingState{listeners: map[string]int32{}, services: map[string]*corev1.Service{}, policies: map[string]*networkingv1.NetworkPolicy{}}
	gateways := &gatewayv1.GatewayList{}
	if err := c.List(ctx, gateways, edge); err != nil {
		t.Fatal(err)
	}
	for i := range gateways.Items {
		for _, l := range gateways.Items[i].Spec.Listeners {
			out.listeners[string(*l.Hostname)] = int32(l.Port)
		}
	}
	services := &corev1.ServiceList{}
	if err := c.List(ctx, services, edge); err != nil {
		t.Fatal(err)
	}
	for i := range services.Items {
		out.services[services.Items[i].Name] = &services.Items[i]
	}
	policies := &networkingv1.NetworkPolicyList{}
	if err := c.List(ctx, policies, edge); err != nil {
		t.Fatal(err)
	}
	for i := range policies.Items {
		out.policies[policies.Items[i].Name] = &policies.Items[i]
	}
	routes := &gatewayv1.TLSRouteList{}
	if err := c.List(ctx, routes, edge); err != nil {
		t.Fatal(err)
	}
	out.routes = len(routes.Items)
	return out
}

// A host a Component lists is carried: a listener, a route, the Services
// and a policy that admits that component's pods. When no Component lists
// it any more, all of it goes, and a host that stays keeps its port.
func TestGrantedHostsAreCarriedAndWithdrawnOnesAreNot(t *testing.T) {
	ctx := context.Background()
	odoo := componentWithHosts("tenant-acme", "odoo", "graph.microsoft.com", "login.microsoftonline.com")
	sync := componentWithHosts("tenant-beta", "sync", "graph.microsoft.com")
	c := fake.NewClientBuilder().WithScheme(outgoingScheme()).
		WithObjects(outgoingClass(), odoo, sync).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	r := &OutgoingGatewayReconciler{Client: c}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	first := readOutgoing(t, c)
	if len(first.listeners) != 2 || first.routes != 2 {
		t.Fatalf("listeners %v, %d routes", first.listeners, first.routes)
	}
	graph := first.services[egress.ServiceName("graph.microsoft.com")]
	if graph == nil || len(graph.Spec.Ports) != 1 || graph.Spec.Ports[0].Port != 443 ||
		graph.Spec.Ports[0].TargetPort.IntVal != first.listeners["graph.microsoft.com"] {
		t.Fatalf("the Service of graph.microsoft.com: %+v", graph)
	}
	fleet := first.policies[egress.FleetPolicyName]
	if fleet == nil || fleet.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("the fleet's policy: %+v", fleet)
	}
	admitted := map[int32]string{}
	for name, p := range first.policies {
		if name == egress.FleetPolicyName {
			continue
		}
		var who []string
		for _, from := range p.Spec.Ingress[0].From {
			who = append(who, from.NamespaceSelector.MatchLabels[corev1.LabelMetadataName]+"/"+from.PodSelector.MatchLabels[appLabel])
		}
		admitted[p.Spec.Ingress[0].Ports[0].Port.IntVal] = strings.Join(who, ",")
	}
	if admitted[first.listeners["graph.microsoft.com"]] != "tenant-acme/odoo,tenant-beta/sync" ||
		admitted[first.listeners["login.microsoftonline.com"]] != "tenant-acme/odoo" {
		t.Errorf("who is admitted to which listener: %v, listeners %v", admitted, first.listeners)
	}

	// odoo loses its grant; sync keeps graph.
	got := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(odoo), got); err != nil {
		t.Fatal(err)
	}
	got.Status.OutgoingHosts = nil
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	second := readOutgoing(t, c)
	if len(second.listeners) != 1 || second.listeners["graph.microsoft.com"] != first.listeners["graph.microsoft.com"] || second.routes != 1 {
		t.Fatalf("after the withdrawal: listeners %v (were %v), %d routes", second.listeners, first.listeners, second.routes)
	}
	if second.services[egress.ServiceName("login.microsoftonline.com")] != nil {
		t.Error("the Service of a host nobody is granted is still there")
	}
	for name, p := range second.policies {
		if name == egress.FleetPolicyName {
			continue
		}
		for _, from := range p.Spec.Ingress[0].From {
			if from.PodSelector.MatchLabels[appLabel] == "odoo" {
				t.Errorf("policy %s still admits the component that lost its grant", name)
			}
		}
	}

	// The last grant goes: nothing is left, the fleet's own policy included.
	if err := c.Get(ctx, client.ObjectKeyFromObject(sync), got); err != nil {
		t.Fatal(err)
	}
	got.Status.OutgoingHosts = nil
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if last := readOutgoing(t, c); len(last.listeners)+len(last.services)+len(last.policies)+last.routes != 0 {
		t.Errorf("with no granted host: %+v", last)
	}
}

// On a cluster without the class nothing is written: a Gateway of a class
// nobody answers for would wait forever.
func TestNothingIsCarriedWithoutTheOutgoingClass(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(outgoingScheme()).
		WithObjects(componentWithHosts("tenant-acme", "odoo", "graph.microsoft.com")).Build()
	res, err := (&OutgoingGatewayReconciler{Client: c}).Reconcile(context.Background(), reconcile.Request{})
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("result %+v, %v; want a later try", res, err)
	}
	if got := readOutgoing(t, c); len(got.listeners)+len(got.services)+len(got.policies) != 0 {
		t.Errorf("written without the class: %+v", got)
	}
}

// An object of the same name that is not this reconciler's is left as it is.
func TestAnObjectOfAnotherOwnerIsNotTaken(t *testing.T) {
	theirs := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: layout.Namespace(layout.Edge), Name: egress.ServiceName("graph.microsoft.com")},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "theirs"}},
	}
	c := fake.NewClientBuilder().WithScheme(outgoingScheme()).
		WithObjects(outgoingClass(), theirs, componentWithHosts("tenant-acme", "odoo", "graph.microsoft.com")).Build()
	if _, err := (&OutgoingGatewayReconciler{Client: c}).Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Service{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(theirs), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Selector["app"] != "theirs" || len(got.Spec.Ports) != 0 {
		t.Errorf("another owner's Service was changed: %+v", got.Spec)
	}
}

// A component reads where its hosts are carried: an entry for its pods'
// host table and the listeners for its policy. A host that is not carried
// yet is one it waits for.
func TestAComponentReadsWhereItsHostsAreCarried(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(outgoingScheme()).
		WithObjects(outgoingClass(), componentWithHosts("tenant-acme", "odoo", "graph.microsoft.com", "graph.microsoft.com:8443")).Build()
	hosts := []string{"graph.microsoft.com", "graph.microsoft.com:8443", "example.org"}

	before, err := readOutgoingWays(ctx, c, hosts)
	if err != nil || len(before.waiting) != 3 || before.rule != nil || len(before.aliases) != 0 {
		t.Fatalf("before anything is carried: %+v, %v", before, err)
	}
	if _, err := (&OutgoingGatewayReconciler{Client: c}).Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	// The API server gives a Service its address; the fake does not.
	svc := &corev1.Service{}
	key := types.NamespacedName{Namespace: layout.Namespace(layout.Edge), Name: egress.ServiceName("graph.microsoft.com")}
	if err := c.Get(ctx, key, svc); err != nil {
		t.Fatal(err)
	}
	svc.Spec.ClusterIP = "10.152.183.32"
	if err := c.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	ways, err := readOutgoingWays(ctx, c, hosts)
	if err != nil {
		t.Fatal(err)
	}
	if len(ways.waiting) != 1 || ways.waiting[0] != "example.org" {
		t.Errorf("waiting for %v, want the host nobody carries", ways.waiting)
	}
	if len(ways.aliases) != 1 || ways.aliases[0].IP != "10.152.183.32" || ways.aliases[0].Hostname != "graph.microsoft.com" {
		t.Errorf("the host table: %+v", ways.aliases)
	}
	if ways.rule == nil || len(ways.rule.Ports) != 2 || ways.rule.To[0].NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != layout.Namespace(layout.Edge) {
		t.Fatalf("the rule: %+v", ways.rule)
	}
	none, err := readOutgoingWays(ctx, c, nil)
	if err != nil || none.rule != nil || len(none.waiting)+len(none.aliases) != 0 {
		t.Errorf("a component with no host: %+v, %v", none, err)
	}
}

// What a component lists is what it was granted now: a request by host
// nobody answered lists nothing, and a name inside the cluster is refused.
func TestAComponentListsTheHostsItWasGranted(t *testing.T) {
	profile := profileRequestingEgress("nextcloud")
	profile.Spec.Requires.Privileges.Egress = append(profile.Spec.Requires.Privileges.Egress, gentianov1alpha1.EgressRequest{
		Name: "outside", Reason: "reads the calendars people connected",
		Hosts: []string{"login.microsoftonline.com", "graph.microsoft.com", "graph.microsoft.com:8443", "vault.kernel-secrets.svc"},
	})
	comp := &gentianov1alpha1.Component{}
	if hosts, refusal := grantedOutgoingHosts(profile, comp, nil); len(hosts) != 0 || refusal != "" {
		t.Errorf("without a grant: %v, %q", hosts, refusal)
	}
	comp.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{{Privilege: "egress/outside", Approver: "tom", ApprovedAt: metav1.Now(), Reason: "agreed"}}
	hosts, refusal := grantedOutgoingHosts(profile, comp, nil)
	if strings.Join(hosts, " ") != "graph.microsoft.com graph.microsoft.com:8443 login.microsoftonline.com" {
		t.Errorf("granted hosts: %v", hosts)
	}
	if !strings.Contains(refusal, "vault.kernel-secrets.svc") {
		t.Errorf("a name inside the cluster was not refused: %q", refusal)
	}
}

// An installed action's hosts are for its runs: the gateway admits the pods
// of its runs in the tenant's namespace for runs, and those pods' policy
// opens the listeners of its hosts.
func TestAnActionsHostsAreForItsRuns(t *testing.T) {
	ctx := context.Background()
	tenant := rightsTenant("acme")
	action := componentWithHosts(tenantNamespaceName(tenant), "ocr", "api.example.org")
	action.Spec.Class = gentianov1alpha1.ComponentClassAction
	c := fake.NewClientBuilder().WithScheme(outgoingScheme()).WithObjects(outgoingClass(), tenant, action).Build()
	if _, err := (&OutgoingGatewayReconciler{Client: c}).Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	state := readOutgoing(t, c)
	var admitted []string
	for name, p := range state.policies {
		if name == egress.FleetPolicyName {
			continue
		}
		for _, from := range p.Spec.Ingress[0].From {
			admitted = append(admitted, from.NamespaceSelector.MatchLabels[corev1.LabelMetadataName]+"/"+from.PodSelector.MatchLabels[dispatcher.LabelAction])
			if from.PodSelector.MatchLabels[appLabel] != "" {
				t.Errorf("an action's host admits pods by the app label: %v", from.PodSelector.MatchLabels)
			}
		}
	}
	if strings.Join(admitted, ",") != layout.TenantRuns("acme")+"/ocr" {
		t.Fatalf("admitted to the action's host: %v", admitted)
	}

	// The runs' side: the action's policy carries the way to the listener.
	ports, err := egress.Allocate(nil, []egress.Host{{Name: "api.example.org", Port: 443}})
	if err != nil {
		t.Fatal(err)
	}
	rule := egress.CallerRule(layout.Namespace(layout.Edge), []egress.Host{{Name: "api.example.org", Port: 443}}, ports)
	runs := &tenantRuns{tenant: tenant, actions: []installedAction{
		{comp: action, profile: &gentianov1alpha1.ComponentProfile{}, runnable: dispatcher.Runnable{Name: "ocr"}, outgoing: rule},
		{comp: action, profile: &gentianov1alpha1.ComponentProfile{}, runnable: dispatcher.Runnable{Name: "plain"}},
	}}
	var found *networkingv1.NetworkPolicy
	for _, p := range runNamespacePolicies(runs, layout.TenantRuns("acme"), tenantNamespaceName(tenant), nil, time.Now()) {
		switch p.Name {
		case "action-ocr":
			found = p
		case "action-plain":
			t.Error("an action with no granted host has a policy of its own")
		}
	}
	if found == nil || found.Spec.PodSelector.MatchLabels[dispatcher.LabelAction] != "ocr" || len(found.Spec.Egress) != 1 ||
		found.Spec.Egress[0].Ports[0].Port.IntVal != ports[egress.Host{Name: "api.example.org", Port: 443}] {
		t.Fatalf("the policy of the action's runs: %+v", found)
	}
}
