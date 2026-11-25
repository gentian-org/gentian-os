/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// The tenant's porter (internal/porter) calls the providers of file-store
// that say where a person's files are, over WebDAV. Each call carries the
// token of the person at the porter's page, and what it may see there is the
// home's answer to it; the network only opens the way. Derived from what is
// installed, with nothing to grant: the porter's pods reach exactly these
// apps' pods, and those admit them.

const (
	porterPolicyName         = "porter"
	porterTargetPolicyPrefix = "porter-target-"
)

func porterTargetPolicyName(app string) string {
	return trimPolicyName(porterTargetPolicyPrefix + app)
}

// porterTargets are the installed file homes the porter calls, sorted.
func (in BuildInput) porterTargets() []string {
	placed := false
	for _, name := range in.Placed {
		if name == gentianov1alpha1.PorterProfile {
			placed = true
		}
	}
	if !placed {
		return nil
	}
	var out []string
	for _, app := range in.Apps {
		p := in.Profiles[app.Profile]
		if p == nil || !deploysPods(p) {
			continue
		}
		for _, c := range p.Spec.Provides {
			if c.Name == "file-store" && c.Files != nil {
				out = append(out, app.Profile)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// PorterNetworkPolicies are the porter's way to the file homes, and their
// way in for it.
func PorterNetworkPolicies(in BuildInput) []*networkingv1.NetworkPolicy {
	targets := in.porterTargets()
	if len(targets) == 0 {
		return nil
	}
	labels := func() map[string]string { return policyLabels(in.TenantName, meta.NetPolicyPorter) }
	porter := in.podsOf(gentianov1alpha1.PorterProfile)
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(targets))
	out := make([]*networkingv1.NetworkPolicy, 0, len(targets)+1)
	for _, target := range targets {
		pods := in.podsOf(target)
		peers = append(peers, networkingv1.NetworkPolicyPeer{PodSelector: &pods})
		out = append(out, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: porterTargetPolicyName(target), Namespace: in.Namespace, Labels: labels()},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: pods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &porter}}}},
			},
		})
	}
	out = append(out, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: porterPolicyName, Namespace: in.Namespace, Labels: labels()},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: porter,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: peers}},
		},
	})
	return out
}
