/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package netpolicy

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gentian-org/gentian-os/internal/meta"
)

const appInternalPolicyPrefix = "app-internal-"

// AppInternalAccessNetworkPolicy allows pods carrying gentianos.io/app=<profile>
// to reach each other within the tenant namespace (e.g. synapse-web → synapse).
func AppInternalAccessNetworkPolicy(tenantName, nsName, appName string) *networkingv1.NetworkPolicy {
	appPeer := networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{meta.AppLabel: appName},
		},
	}
	name := appInternalPolicyName(appName)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyAppInternal),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{meta.AppLabel: appName},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{appPeer}}},
			Egress:  []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{appPeer}}},
		},
	}
}

func appInternalPolicyName(appName string) string {
	name := appInternalPolicyPrefix + appName
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

const appEgressPolicyPrefix = "app-egress-"

// AppEgressNetworkPolicy allows pods carrying gentianos.io/app=<profile> to
// egress by the rules given.
//
// The rules are passed in rather than read from the profile, because what a
// profile DECLARES and what a person GRANTED are different questions and only
// the caller knows the second (AD-5).
func AppEgressNetworkPolicy(tenantName, nsName, appName string, rules []networkingv1.NetworkPolicyEgressRule) *networkingv1.NetworkPolicy {
	name := appEgressPolicyName(appName)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nsName,
			Labels:    policyLabels(tenantName, meta.NetPolicyAppEgress),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{meta.AppLabel: appName},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			},
			Egress: rules,
		},
	}
}

func appEgressPolicyName(appName string) string {
	name := appEgressPolicyPrefix + appName
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}
