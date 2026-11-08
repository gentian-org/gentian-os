/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package kernelnet

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// A small reading of NetworkPolicy ingress, enough to ask of a set of
// policies whether one pod may open a TCP connection to a port of another.
// It is the API's own semantics and no plugin's: a pod no policy selects
// admits everything; a pod some policy selects admits what any rule of any
// policy selecting it admits.

type pod struct {
	namespace string
	nsLabels  map[string]string
	labels    map[string]string
}

func selects(sel *metav1.LabelSelector, set map[string]string) bool {
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false
	}
	return s.Matches(labels.Set(set))
}

// admitted reports whether src may reach dst on the TCP port.
func admitted(policies []*networkingv1.NetworkPolicy, src, dst pod, port int32) bool {
	isolated := false
	for _, p := range policies {
		if p.Namespace != dst.namespace || !selects(&p.Spec.PodSelector, dst.labels) {
			continue
		}
		ingress := false
		for _, t := range p.Spec.PolicyTypes {
			ingress = ingress || t == networkingv1.PolicyTypeIngress
		}
		if !ingress {
			continue
		}
		isolated = true
		for _, rule := range p.Spec.Ingress {
			if portAdmitted(rule.Ports, port) && peerAdmitted(rule.From, p.Namespace, src) {
				return true
			}
		}
	}
	return !isolated
}

func portAdmitted(ports []networkingv1.NetworkPolicyPort, port int32) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p.Protocol != nil && *p.Protocol != corev1.ProtocolTCP {
			continue
		}
		if p.Port == nil || p.Port.IntVal == port {
			return true
		}
	}
	return false
}

func peerAdmitted(from []networkingv1.NetworkPolicyPeer, policyNamespace string, src pod) bool {
	if len(from) == 0 {
		return true
	}
	for _, peer := range from {
		if peer.IPBlock != nil {
			continue // a pod is not an address block here
		}
		if peer.NamespaceSelector == nil {
			if src.namespace != policyNamespace {
				continue
			}
		} else if !selects(peer.NamespaceSelector, src.nsLabels) {
			continue
		}
		if peer.PodSelector == nil || selects(peer.PodSelector, src.labels) {
			return true
		}
	}
	return false
}
