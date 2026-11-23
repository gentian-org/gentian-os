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
	"maps"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/gentian-org/gentian-os/internal/egress"
)

// Hosts this cluster serves itself.
//
// A granted host the cluster's own edge serves is not outside: inside the
// cluster its name resolves, through the hairpin block of CoreDNS
// (coredns_hairpin.go), to the edge's private address, which the gateway for
// outgoing connections may not reach. Such a host is therefore not carried
// by the gateway. The component reaches it as every app reaches the
// cluster's own names: its pods resolve the name to the edge, and its policy
// opens the edge's pods on the port the host's port forwards to, the way an
// address rule to the cluster's public address does (own_public_address.go).
// What those pods serve there is what anybody outside reaches.
//
// A host of an inside entry is the same, at the inside Gateway's address:
// the block maps it there, and the policy opens that Gateway's pods. This
// grant is the only way a component has to such a host.

// hairpinHosts are the host names the hairpin block of a Corefile names,
// each with the address it maps the name to.
func hairpinHosts(corefile string) map[string]string {
	out := map[string]string{}
	in := false
	for _, line := range strings.Split(corefile, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, hairpinBeginMarker):
			in = true
		case strings.HasPrefix(t, hairpinEndMarker):
			in = false
		case in:
			f := strings.Fields(t)
			if len(f) < 2 || net.ParseIP(f[0]) == nil {
				continue
			}
			for _, h := range f[1:] {
				out[strings.ToLower(h)] = f[0]
			}
		}
	}
	return out
}

// ownServedHosts are the hosts the cluster's DNS answers with its own edge
// or its inside Gateway, each with that address, read only where there are
// granted hosts to part. A cluster without the hairpin block serves none of
// them inside.
func ownServedHosts(ctx context.Context, c client.Client, granted []string) (map[string]string, error) {
	if len(granted) == 0 {
		return nil, nil
	}
	name, err := resolveCoreDNSConfigMapName(ctx, c)
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: coreDNSNamespace, Name: name}, cm); err != nil {
		if errors.IsNotFound(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	return hairpinHosts(cm.Data["Corefile"]), nil
}

// splitOwnHosts parts granted hosts into those outside, which the gateway
// carries, and those the cluster serves itself.
func splitOwnHosts(hosts []string, own map[string]string) (outside, inside []string) {
	for _, s := range hosts {
		h, err := egress.ParseHost(s)
		if err == nil {
			if _, ok := own[h.Name]; ok {
				inside = append(inside, s)
				continue
			}
		}
		outside = append(outside, s)
	}
	return outside, inside
}

// ownHostsEgress is the rule that lets a component reach the hosts the
// cluster serves itself at one Service: its pods, by its own selector, on
// the ports the hosts' ports forward to. nil where none of the hosts' ports
// is one the Service serves.
func ownHostsEgress(svc *corev1.Service, inside []string) *networkingv1.NetworkPolicyEgressRule {
	if svc == nil || len(svc.Spec.Selector) == 0 || len(inside) == 0 {
		return nil
	}
	wanted := map[int32]bool{}
	for _, s := range inside {
		if h, err := egress.ParseHost(s); err == nil {
			wanted[h.Port] = true
		}
	}
	var ports []networkingv1.NetworkPolicyPort
	seen := map[string]bool{}
	for i := range svc.Spec.Ports {
		sp := &svc.Spec.Ports[i]
		if (sp.Protocol != "" && sp.Protocol != corev1.ProtocolTCP) || !wanted[sp.Port] {
			continue
		}
		target := sp.TargetPort
		if target.Type == intstr.Int && target.IntVal == 0 {
			target = intstr.FromInt32(sp.Port)
		}
		if seen[target.String()] {
			continue
		}
		seen[target.String()] = true
		proto := corev1.ProtocolTCP
		t := target
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &t})
	}
	if len(ports) == 0 {
		return nil
	}
	return &networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"kubernetes.io/metadata.name": svc.Namespace},
			},
			PodSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(svc.Spec.Selector)},
		}},
		Ports: ports,
	}
}

// ownHostsRules answers ownHostsEgress for each Service a granted host the
// cluster serves itself maps to: the edge's, or the inside Gateway's, by the
// address the hairpin block gives the host. A host whose address is neither
// Service's opens nothing.
func (r *ComponentReconciler) ownHostsRules(ctx context.Context, inside []string, own map[string]string) ([]networkingv1.NetworkPolicyEgressRule, error) {
	if len(inside) == 0 {
		return nil, nil
	}
	var services []corev1.Service
	for _, labels := range []client.MatchingLabels{kernelEdgeServiceLabels(), insideServiceLabels()} {
		list := &corev1.ServiceList{}
		if err := r.List(ctx, list, labels); err != nil {
			return nil, err
		}
		services = append(services, list.Items...)
	}
	return ownHostsRulesFor(services, inside, own), nil
}

// ownHostsRulesFor is ownHostsRules for the Services read.
func ownHostsRulesFor(services []corev1.Service, inside []string, own map[string]string) []networkingv1.NetworkPolicyEgressRule {
	var out []networkingv1.NetworkPolicyEgressRule
	for i := range services {
		svc := &services[i]
		if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
			continue
		}
		var at []string
		for _, s := range inside {
			if h, err := egress.ParseHost(s); err == nil && own[h.Name] == svc.Spec.ClusterIP {
				at = append(at, s)
			}
		}
		if rule := ownHostsEgress(svc, at); rule != nil {
			out = append(out, *rule)
		}
	}
	return out
}

// hairpinChanged passes a CoreDNS ConfigMap whose hairpin hosts changed.
func hairpinChanged() predicate.Predicate {
	hosts := func(o client.Object) map[string]string {
		cm, ok := o.(*corev1.ConfigMap)
		if !ok || cm.Namespace != coreDNSNamespace {
			return nil
		}
		return hairpinHosts(cm.Data["Corefile"])
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return len(hosts(e.Object)) > 0 },
		DeleteFunc:  func(e event.DeleteEvent) bool { return len(hosts(e.Object)) > 0 },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if o, ok := e.ObjectNew.(*corev1.ConfigMap); !ok || o.Namespace != coreDNSNamespace {
				return false
			}
			return !maps.Equal(hosts(e.ObjectOld), hosts(e.ObjectNew))
		},
	}
}

// componentsAskingForEgressOnHairpin wakes every Component whose profile
// asks for egress when the hosts the cluster serves itself change: a host
// that becomes the cluster's own leaves the gateway, and one that stops
// being it goes there.
func componentsAskingForEgressOnHairpin(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return egressAskingComponents(ctx, c)
	})
}
