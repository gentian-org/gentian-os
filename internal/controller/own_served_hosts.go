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

// hairpinHosts are the host names the hairpin block of a Corefile maps to
// the edge.
func hairpinHosts(corefile string) map[string]struct{} {
	out := map[string]struct{}{}
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
				out[strings.ToLower(h)] = struct{}{}
			}
		}
	}
	return out
}

// ownServedHosts are the hosts the cluster's DNS answers with its own edge,
// read only where there are granted hosts to part. A cluster without the
// hairpin block serves none of them inside.
func ownServedHosts(ctx context.Context, c client.Client, granted []string) (map[string]struct{}, error) {
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
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	return hairpinHosts(cm.Data["Corefile"]), nil
}

// splitOwnHosts parts granted hosts into those outside, which the gateway
// carries, and those the cluster serves itself.
func splitOwnHosts(hosts []string, own map[string]struct{}) (outside, inside []string) {
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
// cluster serves itself: the edge Service's pods, by its own selector, on
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

// ownHostsRules reads the edge's Service and answers ownHostsEgress for it.
func (r *ComponentReconciler) ownHostsRules(ctx context.Context, inside []string) ([]networkingv1.NetworkPolicyEgressRule, error) {
	if len(inside) == 0 {
		return nil, nil
	}
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list, kernelEdgeServiceLabels()); err != nil {
		return nil, err
	}
	var out []networkingv1.NetworkPolicyEgressRule
	for i := range list.Items {
		if rule := ownHostsEgress(&list.Items[i], inside); rule != nil {
			out = append(out, *rule)
		}
	}
	return out, nil
}

// hairpinChanged passes a CoreDNS ConfigMap whose hairpin hosts changed.
func hairpinChanged() predicate.Predicate {
	hosts := func(o client.Object) map[string]struct{} {
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
