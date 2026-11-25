/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package egress_test

import (
	"fmt"
	"testing"

	"github.com/gentian-org/gentian-os/internal/egress"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

func host(t *testing.T, s string) egress.Host {
	t.Helper()
	h, err := egress.ParseHost(s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAHostIsANameOutsideWithAPort(t *testing.T) {
	for s, want := range map[string]egress.Host{
		"graph.microsoft.com": {Name: "graph.microsoft.com", Port: 443},
		"example.org:8443":    {Name: "example.org", Port: 8443},
	} {
		got, err := egress.ParseHost(s)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v", s, got, err)
		}
		if got.String() != s {
			t.Errorf("%s is written back as %s", s, got)
		}
	}
	// What the schema cannot tell from a name that is one outside: a
	// Service of a cluster, a name of a private network.
	for _, s := range []string{
		"kubernetes.default.svc", "odoo.tenant-acme.svc.cluster.local", "printer.local",
		"vault.internal", "nas.lan", "example.org:0", "example.org:70000", "localhost", "Example.org", "",
	} {
		if h, err := egress.ParseHost(s); err == nil {
			t.Errorf("%q is taken for a host outside: %+v", s, h)
		}
	}
}

// A caller's rule and a host's Service name the port, so a host keeps the
// one it has for as long as it is granted.
func TestAHostKeepsItsPort(t *testing.T) {
	a, b, c := host(t, "a.example.org"), host(t, "b.example.org"), host(t, "c.example.org")
	first, err := egress.Allocate(nil, []egress.Host{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if first[a] != egress.FirstPort || first[b] != egress.FirstPort+1 {
		t.Fatalf("the first ports: %v", first)
	}
	// a is withdrawn, c is new: b keeps its port, c takes the one a left.
	second, err := egress.Allocate(first, []egress.Host{c, b})
	if err != nil {
		t.Fatal(err)
	}
	if second[b] != first[b] || second[c] != egress.FirstPort || len(second) != 2 {
		t.Fatalf("after a change: %v", second)
	}
	// A port outside the range, or held twice, is not kept.
	odd, err := egress.Allocate(map[egress.Host]int32{a: 443, b: egress.FirstPort, c: egress.FirstPort}, []egress.Host{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int32]bool{}
	for h, p := range odd {
		if p < egress.FirstPort || p > egress.LastPort || seen[p] {
			t.Errorf("%s has port %d: %v", h, p, odd)
		}
		seen[p] = true
	}
}

func TestTheRangeOfPortsIsTheLimit(t *testing.T) {
	var many []egress.Host
	for i := int32(0); i <= egress.LastPort-egress.FirstPort+1; i++ {
		many = append(many, egress.Host{Name: fmt.Sprintf("h%d.example.org", i), Port: 443})
	}
	if _, err := egress.Allocate(nil, many); err == nil {
		t.Fatal("more hosts than ports were given ports")
	}
	if _, err := egress.Allocate(nil, many[:len(many)-1]); err != nil {
		t.Fatalf("as many hosts as ports: %v", err)
	}
}

func TestEveryHostIsAListenerOfItsOwnThatOnlyItsCallersReach(t *testing.T) {
	graph, login, alt := host(t, "graph.microsoft.com"), host(t, "login.microsoftonline.com"), host(t, "graph.microsoft.com:8443")
	odoo := egress.Caller{Namespace: "tenant-acme", PodLabels: map[string]string{"gentianos.io/app": "odoo"}}
	sync := egress.Caller{Namespace: "tenant-beta", PodLabels: map[string]string{"gentianos.io/app": "sync"}}
	grants := []egress.Grant{
		{Host: graph, Callers: []egress.Caller{odoo, sync}},
		{Host: login, Callers: []egress.Caller{odoo}},
		{Host: alt, Callers: []egress.Caller{sync, {Namespace: "tenant-beta"}}},
	}
	ports, err := egress.Allocate(nil, []egress.Host{graph, login, alt})
	if err != nil {
		t.Fatal(err)
	}
	got := egress.Desired("kernel-edge", grants, ports)

	if len(got.Gateways) != 1 || string(got.Gateways[0].Spec.GatewayClassName) != netpolicy.OutgoingGatewayClassName {
		t.Fatalf("the Gateways: %+v", got.Gateways)
	}
	listeners := map[int32]string{}
	for _, l := range got.Gateways[0].Spec.Listeners {
		if l.Hostname == nil || l.TLS == nil || l.TLS.Mode == nil || *l.TLS.Mode != "Passthrough" || l.Protocol != "TLS" {
			t.Errorf("listener %s is not a pass-through for one name: %+v", l.Name, l)
			continue
		}
		if listeners[int32(l.Port)] != "" {
			t.Errorf("port %d is two listeners", l.Port)
		}
		listeners[int32(l.Port)] = string(*l.Hostname)
	}
	for h, p := range ports {
		if listeners[p] != h.Name {
			t.Errorf("%s: the listener on %d is for %q", h, p, listeners[p])
		}
	}

	// A name has one address in a host table, so one Service for all its
	// ports, each at that port's listener.
	callers := map[string]map[int32]int32{}
	for _, s := range got.Services {
		if len(s.Spec.Selector) == 0 {
			continue
		}
		if fmt.Sprint(s.Spec.Selector) != fmt.Sprint(netpolicy.OutgoingProxyPodLabels()) {
			t.Errorf("Service %s selects %v", s.Name, s.Spec.Selector)
		}
		name := s.Annotations[egress.HostAnnotation]
		callers[name] = map[int32]int32{}
		for _, p := range s.Spec.Ports {
			callers[name][p.Port] = p.TargetPort.IntVal
		}
		if s.Name != egress.ServiceName(name) {
			t.Errorf("the Service of %s is %s", name, s.Name)
		}
	}
	if len(callers) != 2 || callers["graph.microsoft.com"][443] != ports[graph] || callers["graph.microsoft.com"][8443] != ports[alt] ||
		callers["login.microsoftonline.com"][443] != ports[login] {
		t.Errorf("the callers' Services: %v, ports %v", callers, ports)
	}

	// The upstream is the name, never an address.
	if len(got.EndpointSlices) != 2 {
		t.Fatalf("the endpoints: %+v", got.EndpointSlices)
	}
	for _, s := range got.EndpointSlices {
		if s.AddressType != "FQDN" || len(s.Endpoints) != 1 || s.Endpoints[0].Addresses[0] != s.Annotations[egress.HostAnnotation] {
			t.Errorf("endpoint %s: %+v", s.Name, s)
		}
	}
	if len(got.Routes) != 3 {
		t.Fatalf("the routes: %d", len(got.Routes))
	}
	for _, r := range got.Routes {
		if len(r.Spec.Hostnames) != 1 || len(r.Spec.ParentRefs) != 1 || r.Spec.ParentRefs[0].SectionName == nil {
			t.Errorf("route %s is not for one name on one listener: %+v", r.Name, r.Spec)
		}
	}

	// Each listener admits the pods that were granted its host. A caller
	// with no labels would be a whole namespace and is left out.
	admitted := map[int32][]string{}
	for _, p := range got.NetworkPolicies {
		if fmt.Sprint(p.Spec.PodSelector.MatchLabels) != fmt.Sprint(netpolicy.OutgoingProxyPodLabels()) || len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].Ports) != 1 {
			t.Fatalf("policy %s: %+v", p.Name, p.Spec)
		}
		port := p.Spec.Ingress[0].Ports[0].Port.IntVal
		for _, from := range p.Spec.Ingress[0].From {
			if from.NamespaceSelector == nil || from.PodSelector == nil || len(from.PodSelector.MatchLabels) == 0 {
				t.Errorf("policy %s admits more than pods of a namespace: %+v", p.Name, from)
				continue
			}
			admitted[port] = append(admitted[port], from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]+"/"+from.PodSelector.MatchLabels["gentianos.io/app"])
		}
	}
	for h, want := range map[egress.Host]string{graph: "[tenant-acme/odoo tenant-beta/sync]", login: "[tenant-acme/odoo]", alt: "[tenant-beta/sync]"} {
		if fmt.Sprint(admitted[ports[h]]) != want {
			t.Errorf("%s admits %v, want %s", h, admitted[ports[h]], want)
		}
	}
}

func TestAGatewayHoldsSixtyFourListeners(t *testing.T) {
	var grants []egress.Grant
	var hosts []egress.Host
	for i := 0; i < 130; i++ {
		h := egress.Host{Name: fmt.Sprintf("h%03d.example.org", i), Port: 443}
		hosts = append(hosts, h)
		grants = append(grants, egress.Grant{Host: h})
	}
	ports, err := egress.Allocate(nil, hosts)
	if err != nil {
		t.Fatal(err)
	}
	got := egress.Desired("kernel-edge", grants, ports)
	if len(got.Gateways) != 3 {
		t.Fatalf("%d Gateways for 130 hosts", len(got.Gateways))
	}
	names := map[string]bool{}
	for _, gw := range got.Gateways {
		if len(gw.Spec.Listeners) > int(egress.ListenersPerGateway) || names[gw.Name] {
			t.Errorf("Gateway %s has %d listeners", gw.Name, len(gw.Spec.Listeners))
		}
		names[gw.Name] = true
	}
	// A host that nobody may call has a listener and no policy: nothing
	// reaches it.
	if len(got.NetworkPolicies) != 0 {
		t.Errorf("%d policies for hosts with no caller", len(got.NetworkPolicies))
	}
}

func TestACallerReachesTheListenersOfItsHostsAndNothingElse(t *testing.T) {
	a, b, c := host(t, "a.example.org"), host(t, "b.example.org"), host(t, "c.example.org")
	ports, _ := egress.Allocate(nil, []egress.Host{a, b, c})
	rule := egress.CallerRule("kernel-edge", []egress.Host{c, a}, ports)
	if rule == nil || len(rule.To) != 1 || rule.To[0].IPBlock != nil ||
		rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kernel-edge" ||
		fmt.Sprint(rule.To[0].PodSelector.MatchLabels) != fmt.Sprint(netpolicy.OutgoingProxyPodLabels()) {
		t.Fatalf("the rule: %+v", rule)
	}
	if len(rule.Ports) != 2 || rule.Ports[0].Port.IntVal != ports[a] || rule.Ports[1].Port.IntVal != ports[c] {
		t.Errorf("the ports: %+v, want those of a and c in %v", rule.Ports, ports)
	}
	if egress.CallerRule("kernel-edge", nil, ports) != nil || egress.CallerRule("kernel-edge", []egress.Host{host(t, "d.example.org")}, ports) != nil {
		t.Error("a caller with no host that has a port is opened a way")
	}
}

// The fleet resolves a granted name itself. Whatever the name resolves to,
// the fleet reaches public addresses only, so it is no way into the cluster.
func TestTheFleetReachesPublicAddressesOnTheGrantedPortsOnly(t *testing.T) {
	controller := map[string]string{"control-plane": "envoy-gateway"}
	p := egress.FleetPolicy("kernel-edge", controller, 18000, []egress.Host{host(t, "a.example.org"), host(t, "b.example.org:8443"), host(t, "c.example.org")})
	if fmt.Sprint(p.Spec.PodSelector.MatchLabels) != fmt.Sprint(netpolicy.OutgoingProxyPodLabels()) || len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != "Egress" {
		t.Fatalf("the policy: %+v", p.Spec)
	}
	var outside int
	for _, rule := range p.Spec.Egress {
		for _, to := range rule.To {
			if to.IPBlock == nil {
				continue
			}
			outside++
			if to.IPBlock.CIDR != "0.0.0.0/0" {
				t.Errorf("an address block %s", to.IPBlock.CIDR)
			}
			for _, private := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "127.0.0.0/8"} {
				found := false
				for _, ex := range to.IPBlock.Except {
					found = found || ex == private
				}
				if !found {
					t.Errorf("%s is not left out", private)
				}
			}
			if len(rule.Ports) != 2 || rule.Ports[0].Port.IntVal != 443 || rule.Ports[1].Port.IntVal != 8443 {
				t.Errorf("the ports outside: %+v", rule.Ports)
			}
		}
	}
	if outside != 1 {
		t.Errorf("%d rules to addresses", outside)
	}
	// With no host, nothing outside at all.
	for _, rule := range egress.FleetPolicy("kernel-edge", controller, 18000, nil).Spec.Egress {
		for _, to := range rule.To {
			if to.IPBlock != nil {
				t.Error("a fleet with no host reaches addresses")
			}
		}
	}
}
