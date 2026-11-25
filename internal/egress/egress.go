/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package egress builds what carries a component's connections to hosts
// outside the cluster: the Gateways for outgoing connections, a listener, a
// route and two Services per granted host, and the rules that say which pods
// reach which host.
//
// A granted host is a listener of its own on a port of its own. A
// NetworkPolicy selects pods and ports, never a name, so the port is what
// makes "this component may reach this host" a rule the network plugin
// enforces: the caller's policy opens the fleet's pods on the ports of its
// hosts, and the fleet's policy admits on each port the pods that were
// granted the host. The listener passes a connection on only when it opens
// with that host's name, and the fleet resolves the name itself.
//
// Nothing here reads the cluster. The controller gathers the grants and the
// ports already given out, and applies what Desired returns.
package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
)

const (
	// FirstPort and LastPort bound the listener ports. A block of
	// ListenersPerGateway ports is one Gateway: the Gateway API holds a
	// Gateway to 64 listeners.
	FirstPort           int32 = 20000
	LastPort            int32 = 27999
	ListenersPerGateway int32 = 64

	// HostLabel is on every object made for one host, with the host's key
	// as its value; HostAnnotation carries the host as it was written.
	HostLabel      = "gentianos.io/outgoing-host"
	HostAnnotation = "gentianos.io/outgoing-host-name"
	// ManagedLabel marks every object of this package, the Gateways and the
	// fleet's own policy included.
	ManagedLabel = "gentianos.io/outgoing"

	defaultPort int32 = 443
)

// Host is one host outside the cluster, as a request names it.
type Host struct {
	Name string
	Port int32
}

// ParseHost reads "name" or "name:port". The schema holds a request to that
// shape; this refuses what the schema cannot tell: a name that is the
// cluster's own.
func ParseHost(s string) (Host, error) {
	name, port := s, defaultPort
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		p, err := strconv.ParseInt(s[i+1:], 10, 32)
		if err != nil || p < 1 || p > 65535 {
			return Host{}, fmt.Errorf("%q: the port is not a port", s)
		}
		name, port = s[:i], int32(p)
	}
	if name == "" || !strings.Contains(name, ".") || name != strings.ToLower(name) {
		return Host{}, fmt.Errorf("%q: not a host name", s)
	}
	for _, inside := range insideSuffixes {
		if strings.HasSuffix(name, inside) {
			return Host{}, fmt.Errorf("%q: a name inside a cluster or a private network is not a host outside", s)
		}
	}
	return Host{Name: name, Port: port}, nil
}

// insideSuffixes end the names that are never a host outside: a Service's
// names, and the top-level names reserved for private use.
var insideSuffixes = []string{".svc", ".local", ".internal", ".localhost", ".home.arpa", ".lan"}

// String is the host as a request names it, the port left out where it is
// the default.
func (h Host) String() string {
	if h.Port == defaultPort {
		return h.Name
	}
	return h.Name + ":" + strconv.Itoa(int(h.Port))
}

// Key names the host's objects: ten hex digits of the host and its port.
// A host name is up to 253 characters and holds dots, which the names of a
// listener and a Service do not.
func (h Host) Key() string {
	sum := sha256.Sum256([]byte(h.Name + ":" + strconv.Itoa(int(h.Port))))
	return hex.EncodeToString(sum[:5])
}

// Caller is a set of pods that was granted a host.
type Caller struct {
	Namespace string
	PodLabels map[string]string
}

// Grant is one host and everybody who was granted it.
type Grant struct {
	Host    Host
	Callers []Caller
}

// Allocate gives every wanted host a listener port. A host keeps the port it
// has, since the callers' rules and the Services name it; a new host takes
// the lowest port nobody holds. Ports of hosts no longer wanted are free
// again. It fails when the range is used up.
func Allocate(existing map[Host]int32, wanted []Host) (map[Host]int32, error) {
	out := make(map[Host]int32, len(wanted))
	taken := map[int32]bool{}
	sorted := append([]Host(nil), wanted...)
	sort.Slice(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })
	for _, h := range sorted {
		if p, ok := existing[h]; ok && p >= FirstPort && p <= LastPort && !taken[p] {
			out[h], taken[p] = p, true
		}
	}
	next := FirstPort
	for _, h := range sorted {
		if _, ok := out[h]; ok {
			continue
		}
		for next <= LastPort && taken[next] {
			next++
		}
		if next > LastPort {
			return nil, fmt.Errorf("no listener port is left for %s: %d hosts are granted", h, len(out))
		}
		out[h], taken[next] = next, true
	}
	return out, nil
}

func less(a, b Host) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Port < b.Port
}

// Objects are what the edge namespace holds for the granted hosts.
type Objects struct {
	Gateways        []*gatewayv1.Gateway
	Routes          []*gatewayv1.TLSRoute
	Services        []*corev1.Service
	EndpointSlices  []*discoveryv1.EndpointSlice
	NetworkPolicies []*networkingv1.NetworkPolicy
}

// nameKey names the two Services of a host name, which are one pair for all
// its ports: a host table gives a name one address.
func nameKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:5])
}

// ServiceName is the Service a caller's host table points the name at. It
// has one port for each granted port of the name.
func ServiceName(name string) string { return "out-" + nameKey(name) }

// upstreamName is the Service that stands for the host outside.
func upstreamName(name string) string { return "out-" + nameKey(name) + "-host" }

// hostObjectName names the route and the policy of one host and port.
func hostObjectName(h Host) string { return "out-" + h.Key() }

func gatewayName(port int32) string {
	return "outgoing-" + strconv.Itoa(int((port-FirstPort)/ListenersPerGateway))
}

// Desired builds every object for the grants, in namespace, with the ports
// Allocate gave. A grant without a port is left out.
func Desired(namespace string, grants []Grant, ports map[Host]int32) Objects {
	sorted := append([]Grant(nil), grants...)
	sort.Slice(sorted, func(i, j int) bool { return ports[sorted[i].Host] < ports[sorted[j].Host] })

	var out Objects
	gateways := map[string]*gatewayv1.Gateway{}
	byName := map[string]*servicePair{}
	passthrough := gatewayv1.TLSModePassthrough
	same := gatewayv1.NamespacesFromSame
	tcp := corev1.ProtocolTCP
	for i := range sorted {
		h := sorted[i].Host
		port, ok := ports[h]
		if !ok {
			continue
		}
		labels := map[string]string{ManagedLabel: "true", HostLabel: h.Key()}
		meta := func(name string) metav1.ObjectMeta {
			return metav1.ObjectMeta{
				Name: name, Namespace: namespace,
				Labels:      copyOf(labels),
				Annotations: map[string]string{HostAnnotation: h.String()},
			}
		}

		gw := gateways[gatewayName(port)]
		if gw == nil {
			gw = &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name: gatewayName(port), Namespace: namespace,
					Labels: map[string]string{ManagedLabel: "true"},
				},
				Spec: gatewayv1.GatewaySpec{GatewayClassName: netpolicy.OutgoingGatewayClassName},
			}
			gateways[gw.Name] = gw
			out.Gateways = append(out.Gateways, gw)
		}
		hostname := gatewayv1.Hostname(h.Name)
		section := gatewayv1.SectionName("h-" + h.Key())
		gw.Spec.Listeners = append(gw.Spec.Listeners, gatewayv1.Listener{
			Name:     section,
			Port:     port,
			Protocol: gatewayv1.TLSProtocolType,
			Hostname: &hostname,
			TLS:      &gatewayv1.ListenerTLSConfig{Mode: &passthrough},
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{From: &same},
			},
		})

		// What a caller connects to: the name's own ports, on the fleet's
		// pods at each port's listener. And what the fleet connects to: the
		// host outside, by name -- a Service without a selector and an
		// endpoint that is a name, which the proxies resolve.
		pair := byName[h.Name]
		if pair == nil {
			nameMeta := func(name string) metav1.ObjectMeta {
				return metav1.ObjectMeta{
					Name: name, Namespace: namespace,
					Labels:      map[string]string{ManagedLabel: "true", HostLabel: nameKey(h.Name)},
					Annotations: map[string]string{HostAnnotation: h.Name},
				}
			}
			pair = &servicePair{
				callers:  &corev1.Service{ObjectMeta: nameMeta(ServiceName(h.Name)), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: netpolicy.OutgoingProxyPodLabels()}},
				upstream: &corev1.Service{ObjectMeta: nameMeta(upstreamName(h.Name)), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}},
				slice: &discoveryv1.EndpointSlice{
					ObjectMeta:  nameMeta(upstreamName(h.Name)),
					AddressType: discoveryv1.AddressTypeFQDN,
					Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{h.Name}}},
				},
			}
			pair.slice.Labels[discoveryv1.LabelServiceName] = upstreamName(h.Name)
			byName[h.Name] = pair
			out.Services = append(out.Services, pair.callers, pair.upstream)
			out.EndpointSlices = append(out.EndpointSlices, pair.slice)
		}
		portName := "p" + strconv.Itoa(int(h.Port))
		hostPort := h.Port
		pair.callers.Spec.Ports = append(pair.callers.Spec.Ports, corev1.ServicePort{
			Name: portName, Protocol: tcp, Port: h.Port, TargetPort: intstr.FromInt32(port),
		})
		pair.upstream.Spec.Ports = append(pair.upstream.Spec.Ports, corev1.ServicePort{
			Name: portName, Protocol: tcp, Port: h.Port, TargetPort: intstr.FromInt32(h.Port),
		})
		pair.slice.Ports = append(pair.slice.Ports, discoveryv1.EndpointPort{Name: strPtr(portName), Protocol: &tcp, Port: &hostPort})

		backendPort := h.Port
		out.Routes = append(out.Routes, &gatewayv1.TLSRoute{
			ObjectMeta: meta(hostObjectName(h)),
			Spec: gatewayv1.TLSRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
					Name: gatewayv1.ObjectName(gw.Name), SectionName: &section,
				}}},
				Hostnames: []gatewayv1.Hostname{hostname},
				Rules: []gatewayv1.TLSRouteRule{{BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(upstreamName(h.Name)), Port: &backendPort,
					},
				}}}},
			},
		})

		// Who reaches the listener: the pods that were granted the host.
		if peers := callerPeers(sorted[i].Callers); len(peers) > 0 {
			listener := intstr.FromInt32(port)
			out.NetworkPolicies = append(out.NetworkPolicies, &networkingv1.NetworkPolicy{
				ObjectMeta: meta(hostObjectName(h)),
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: netpolicy.OutgoingProxyPodLabels()},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					Ingress: []networkingv1.NetworkPolicyIngressRule{{
						From:  peers,
						Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &listener}},
					}},
				},
			})
		}
	}
	return out
}

// servicePair are the two Services of one host name and the endpoint of the
// second.
type servicePair struct {
	callers, upstream *corev1.Service
	slice             *discoveryv1.EndpointSlice
}

func callerPeers(callers []Caller) []networkingv1.NetworkPolicyPeer {
	sorted := append([]Caller(nil), callers...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Namespace != sorted[j].Namespace {
			return sorted[i].Namespace < sorted[j].Namespace
		}
		return fmt.Sprint(sorted[i].PodLabels) < fmt.Sprint(sorted[j].PodLabels)
	})
	var peers []networkingv1.NetworkPolicyPeer
	for _, c := range sorted {
		// A caller with no labels would be every pod of its namespace.
		if c.Namespace == "" || len(c.PodLabels) == 0 {
			continue
		}
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: c.Namespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: copyOf(c.PodLabels)},
		})
	}
	return peers
}

// CallerRule is what a caller's own policy opens: the fleet's pods in the
// edge namespace, on the listener ports of the hosts it was granted, and
// nothing else outside. Nil when no host has a port.
func CallerRule(edgeNamespace string, hosts []Host, ports map[Host]int32) *networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	var listeners []int32
	for _, h := range hosts {
		if p, ok := ports[h]; ok {
			listeners = append(listeners, p)
		}
	}
	if len(listeners) == 0 {
		return nil
	}
	sort.Slice(listeners, func(i, j int) bool { return listeners[i] < listeners[j] })
	rule := &networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: edgeNamespace}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: netpolicy.OutgoingProxyPodLabels()},
		}},
	}
	for _, p := range listeners {
		port := intstr.FromInt32(p)
		rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	return rule
}

// notPublic is every address that is not a public IPv4 unicast address: the
// pod, Service and node ranges of a cluster, loopback, the link-local range
// the cloud metadata address is in, multicast and reserved space.
var notPublic = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"224.0.0.0/3",
}

// FleetPolicyName is the fleet's own policy for what it may call.
const FleetPolicyName = "outgoing-fleet"

// FleetPolicy is what the fleet itself may call: the cluster's DNS, Envoy
// Gateway for its configuration, and public addresses on the ports of the
// granted hosts. A granted name that resolves to an address inside the
// cluster or a private network is therefore not reached, whatever the name
// says, and the fleet is no way from a component into the cluster.
//
// No IPv6 block is named, so nothing leaves over IPv6.
func FleetPolicy(namespace string, controllerPods map[string]string, configPort int32, hosts []Host) *networkingv1.NetworkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns := intstr.FromInt32(53)
	config := intstr.FromInt32(configPort)
	egress := []networkingv1.NetworkPolicyEgressRule{
		{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}}},
		{
			To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: copyOf(controllerPods)}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &config}},
		},
	}
	seen := map[int32]bool{}
	var outside []int32
	for _, h := range hosts {
		if !seen[h.Port] {
			seen[h.Port] = true
			outside = append(outside, h.Port)
		}
	}
	sort.Slice(outside, func(i, j int) bool { return outside[i] < outside[j] })
	if len(outside) > 0 {
		rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: append([]string(nil), notPublic...)},
		}}}
		for _, p := range outside {
			port := intstr.FromInt32(p)
			rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
		}
		egress = append(egress, rule)
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: FleetPolicyName, Namespace: namespace,
			Labels: map[string]string{ManagedLabel: "true"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: netpolicy.OutgoingProxyPodLabels()},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

func copyOf(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func strPtr(s string) *string { return &s }
