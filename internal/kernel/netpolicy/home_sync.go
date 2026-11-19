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

// The tenant's home sync (internal/homesync) calls two kinds of app of its
// tenant: the apps that declare homeSync, over their API, and the providers
// of the homes' contracts, over DAV. Each call carries a person's token, and
// what it may do there is the app's answer to it; the network only opens the
// way. Derived from what is installed, with nothing to grant: the home sync's
// pods reach exactly these apps' pods, and those admit them.

const (
	homeSyncPolicyName        = "homesync"
	homeSyncTargetPolicyPrefx = "homesync-target-"
)

func homeSyncTargetPolicyName(app string) string {
	return trimPolicyName(homeSyncTargetPolicyPrefx + app)
}

// homeSyncTargets are the installed apps the home sync calls, sorted.
func (in BuildInput) homeSyncTargets() []string {
	placed := false
	for _, name := range in.Placed {
		if name == gentianov1alpha1.HomeSyncProfile {
			placed = true
		}
	}
	if !placed {
		return nil
	}
	set := map[string]bool{}
	for _, app := range in.Apps {
		p := in.Profiles[app.Profile]
		if p == nil || !deploysPods(p) {
			continue
		}
		if p.Spec.HomeSync != nil {
			set[app.Profile] = true
		}
		for _, c := range p.Spec.Provides {
			if c.Name == "calendar-store" || c.Name == "contacts-store" {
				set[app.Profile] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// HomeSyncNetworkPolicies are the home sync's way to the apps it calls, and
// their way in for it.
func HomeSyncNetworkPolicies(in BuildInput) []*networkingv1.NetworkPolicy {
	targets := in.homeSyncTargets()
	if len(targets) == 0 {
		return nil
	}
	labels := func() map[string]string { return policyLabels(in.TenantName, meta.NetPolicyHomeSync) }
	syncer := in.podsOf(gentianov1alpha1.HomeSyncProfile)
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(targets))
	out := make([]*networkingv1.NetworkPolicy, 0, len(targets)+1)
	for _, target := range targets {
		pods := in.podsOf(target)
		peers = append(peers, networkingv1.NetworkPolicyPeer{PodSelector: &pods})
		out = append(out, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: homeSyncTargetPolicyName(target), Namespace: in.Namespace, Labels: labels()},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: pods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &syncer}}}},
			},
		})
	}
	out = append(out, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: homeSyncPolicyName, Namespace: in.Namespace, Labels: labels()},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: syncer,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: peers}},
		},
	})
	return out
}
