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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The realm's signing keys, for a component that is told the issuer.
//
// A component told the realm's issuer (valueMapping.platform.issuerKey)
// verifies tokens against it, and fetches the realm's public keys to do so.
// From inside the cluster the issuer's address is the edge: a connection to
// it is rewritten to the Envoy pods behind the edge's Service, at the port
// its https port forwards to. A component whose own exposure hands it a token
// has the edge namespace already (componentEgressNamespaces); one that is
// handed a person's token by another component has no exposure, and without
// this rule it times out on the keys.
//
// The rule names the edge Service's pods, by the Service's own selector, on
// that one port. What they serve there is what the edge serves to anybody
// outside; nothing else of the edge namespace is opened.

// issuerHTTPSPort is the port the issuer is asked on.
const issuerHTTPSPort = 443

// wantsIssuerKeys reports whether the profile is told the realm's issuer and
// no exposure of its own already takes it to the edge.
func wantsIssuerKeys(profile *gentianov1alpha1.ComponentProfile) bool {
	m := platformMapping(profile)
	if m == nil || m.IssuerKey == "" {
		return false
	}
	for i := range profile.Spec.Expose {
		if e := &profile.Spec.Expose[i]; e.ForwardToken || e.ExchangeToken {
			return false
		}
	}
	return true
}

// issuerKeysEgress is the rule to the pods of one edge Service on the port
// its https port forwards to, or nil where the Service has no selector or no
// https port.
func issuerKeysEgress(svc *corev1.Service) *networkingv1.NetworkPolicyEgressRule {
	if svc == nil || len(svc.Spec.Selector) == 0 {
		return nil
	}
	var ports []networkingv1.NetworkPolicyPort
	for i := range svc.Spec.Ports {
		sp := &svc.Spec.Ports[i]
		if sp.Port != issuerHTTPSPort || (sp.Protocol != "" && sp.Protocol != corev1.ProtocolTCP) {
			continue
		}
		target := sp.TargetPort
		if target.Type == intstr.Int && target.IntVal == 0 {
			target = intstr.FromInt32(sp.Port)
		}
		tcp := corev1.ProtocolTCP
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &target})
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

// issuerKeysRule reads the edge's Service and answers the rule for each.
func (r *ComponentReconciler) issuerKeysRule(ctx context.Context) ([]networkingv1.NetworkPolicyEgressRule, error) {
	list := &corev1.ServiceList{}
	if err := r.List(ctx, list, kernelEdgeServiceLabels()); err != nil {
		return nil, err
	}
	var out []networkingv1.NetworkPolicyEgressRule
	for i := range list.Items {
		if rule := issuerKeysEgress(&list.Items[i]); rule != nil {
			out = append(out, *rule)
		}
	}
	return out, nil
}
