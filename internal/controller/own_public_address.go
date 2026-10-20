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
	"net"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The cluster's own public address, reached from inside it.
//
// An app granted the internet -- an egress rule whose ipBlock is the public
// address space -- means every public address, this cluster's among them: a
// company app in one tenant reading another tenant's API by its public name
// does nothing else. From a pod, that address never leaves the cluster.
// kube-proxy rewrites a connection to the edge's LoadBalancer address
// straight to the Envoy pods behind it, at their pod addresses and container
// ports, and the CNI applies the NetworkPolicy to what it was rewritten to:
// a private address the rule excepts, on a port (10443) the rule does not
// name. The connection is dropped and the app times out, though the rule
// allows the address it asked for.
//
// So where a rule of the component -- granted, or the store's way out --
// admits the edge Service's public address on one of its ports, the component is also let reach that Service's pods -- by the
// Service's own selector, in its namespace -- on the port that one forwards
// to, and no other. What those pods serve there is what the LoadBalancer
// serves to anybody outside; nothing becomes reachable that the internet
// cannot reach.

// ownPublicAddressEgress is the rule that lets the given rules reach the
// edge's public address from inside, or nil where they do not admit it or
// the edge has no LoadBalancer address.
func ownPublicAddressEgress(svc *corev1.Service, granted []networkingv1.NetworkPolicyEgressRule) *networkingv1.NetworkPolicyEgressRule {
	if svc == nil || len(svc.Spec.Selector) == 0 {
		return nil
	}
	addrs := serviceExternalAddresses(svc)
	if len(addrs) == 0 {
		return nil
	}
	var ports []networkingv1.NetworkPolicyPort
	seen := map[string]bool{}
	for i := range svc.Spec.Ports {
		sp := &svc.Spec.Ports[i]
		proto := sp.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		if !anyRuleAdmits(granted, addrs, proto, sp.Port) {
			continue
		}
		target := sp.TargetPort
		if target.Type == intstr.Int && target.IntVal == 0 {
			target = intstr.FromInt32(sp.Port)
		}
		key := string(proto) + "/" + target.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		p := proto
		t := target
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &p, Port: &t})
	}
	if len(ports) == 0 {
		return nil
	}
	selector := map[string]string{}
	for k, v := range svc.Spec.Selector {
		selector[k] = v
	}
	return &networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": svc.Namespace},
			},
			PodSelector: &metav1.LabelSelector{MatchLabels: selector},
		}},
		Ports: ports,
	}
}

// serviceExternalAddresses are the addresses the Service answers at from
// outside: its LoadBalancer's and any external IPs it names.
func serviceExternalAddresses(svc *corev1.Service) []net.IP {
	var out []net.IP
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if ip := net.ParseIP(in.IP); ip != nil {
			out = append(out, ip)
		}
	}
	for _, s := range svc.Spec.ExternalIPs {
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

// anyRuleAdmits reports whether one of the rules lets a connection to one of
// the addresses on the given protocol and port through.
func anyRuleAdmits(rules []networkingv1.NetworkPolicyEgressRule, addrs []net.IP, proto corev1.Protocol, port int32) bool {
	for i := range rules {
		if ruleAdmitsPort(&rules[i], proto, port) && ruleAdmitsAnyAddress(&rules[i], addrs) {
			return true
		}
	}
	return false
}

// ruleAdmitsPort: a rule without ports admits every port.
func ruleAdmitsPort(rule *networkingv1.NetworkPolicyEgressRule, proto corev1.Protocol, port int32) bool {
	if len(rule.Ports) == 0 {
		return true
	}
	for _, p := range rule.Ports {
		pp := corev1.ProtocolTCP
		if p.Protocol != nil {
			pp = *p.Protocol
		}
		if pp != proto {
			continue
		}
		if p.Port == nil {
			return true
		}
		if p.Port.Type != intstr.Int {
			continue // a named port names a pod's port, never an address's
		}
		lo, hi := p.Port.IntVal, p.Port.IntVal
		if p.EndPort != nil {
			hi = *p.EndPort
		}
		if port >= lo && port <= hi {
			return true
		}
	}
	return false
}

// ruleAdmitsAnyAddress: only an ipBlock peer speaks of addresses outside the
// cluster; a rule with no peers admits everything.
func ruleAdmitsAnyAddress(rule *networkingv1.NetworkPolicyEgressRule, addrs []net.IP) bool {
	if len(rule.To) == 0 {
		return true
	}
	for _, peer := range rule.To {
		if peer.IPBlock == nil {
			continue
		}
		_, block, err := net.ParseCIDR(peer.IPBlock.CIDR)
		if err != nil {
			continue
		}
	addresses:
		for _, ip := range addrs {
			if !block.Contains(ip) {
				continue
			}
			for _, ex := range peer.IPBlock.Except {
				if _, exBlock, err := net.ParseCIDR(ex); err == nil && exBlock.Contains(ip) {
					continue addresses
				}
			}
			return true
		}
	}
	return false
}

// ownPublicAddressRule reads the edge's Service and answers the rule for the
// component's rules. A cluster whose edge has no Service yet, or none with a
// public address, gets nothing: there is no own address to reach.
func (r *ComponentReconciler) ownPublicAddressRule(ctx context.Context, granted []networkingv1.NetworkPolicyEgressRule) ([]networkingv1.NetworkPolicyEgressRule, error) {
	if len(granted) == 0 {
		return nil, nil
	}
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list, kernelEdgeServiceLabels()); err != nil {
		return nil, err
	}
	var out []networkingv1.NetworkPolicyEgressRule
	for i := range list.Items {
		if rule := ownPublicAddressEgress(&list.Items[i], granted); rule != nil {
			out = append(out, *rule)
		}
	}
	return out, nil
}

// componentsAskingForEgress maps a change of the edge's Service to every
// component whose profile asks for egress or is told the store: the rule above follows the
// Service's address, ports and selector, and a LoadBalancer address arrives
// after the components that would reach it were reconciled.
func componentsAskingForEgress(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		profiles := &gentianov1alpha1.ComponentProfileList{}
		if err := c.List(ctx, profiles); err != nil {
			return nil
		}
		asking := map[string]struct{}{}
		for i := range profiles.Items {
			if p := profiles.Items[i].Privileges(); (p != nil && len(p.Egress) > 0) || wantsStore(&profiles.Items[i]) {
				asking[profiles.Items[i].Name] = struct{}{}
			}
		}
		if len(asking) == 0 {
			return nil
		}
		comps := &gentianov1alpha1.ComponentList{}
		if err := c.List(ctx, comps); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range comps.Items {
			if _, ok := asking[comps.Items[i].Spec.ProfileRef.Name]; ok {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&comps.Items[i])})
			}
		}
		return out
	})
}

// edgeAddressChanged passes an edge Service whose public address, ports or
// selector changed, and nothing else of any Service.
func edgeAddressChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isKernelEdgeService(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return isKernelEdgeService(e.Object) },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !isKernelEdgeService(e.ObjectNew) {
				return false
			}
			o, okO := e.ObjectOld.(*corev1.Service)
			n, okN := e.ObjectNew.(*corev1.Service)
			if !okO || !okN {
				return true
			}
			return !equality.Semantic.DeepEqual(o.Status.LoadBalancer, n.Status.LoadBalancer) ||
				!equality.Semantic.DeepEqual(o.Spec.Ports, n.Spec.Ports) ||
				!equality.Semantic.DeepEqual(o.Spec.ExternalIPs, n.Spec.ExternalIPs) ||
				!equality.Semantic.DeepEqual(o.Spec.Selector, n.Spec.Selector)
		},
	}
}
