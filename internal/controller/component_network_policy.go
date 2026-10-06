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
	"fmt"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/security"
)

// A tenant namespace is closed by default (netpolicy.BaselineNetworkPolicy):
// DNS, the API server, and ingress from the edge. A component reaches what
// its requirements were fulfilled with and nothing else, and that is written
// down as a policy of its own beside the baseline -- the same shape an app's
// kernel-access policy has, but derived from the ComponentProfile.
//
// Beyond that, a profile may declare egress privileges. Those are requests:
// they reach this policy only once somebody granted them by name, which is
// what security.GrantedEgressRules resolves.

// componentInstanceLabel is how a chart names the pods of one release, and
// therefore how the operator selects a component's pods without knowing
// the chart.
const componentInstanceLabel = "app.kubernetes.io/instance"

func componentNetworkPolicyName(comp *gentianov1alpha1.Component) string {
	return "component-" + comp.Name
}

// componentEgressNamespaces lists the namespaces a component's pods may
// reach: the one its database requirement was fulfilled from; the edge,
// when an exposure forwards the token, because a component that is handed
// a token verifies it against the issuer, and the issuer is reached through
// the edge (the baseline's 443 allow names a CIDR, which Calico matches
// against the Envoy pod's own port, never 443); and for the desktop the
// director's, which the BFF relays to (ui-restructure.md §1).
func (r *ComponentReconciler) componentEgressNamespaces(profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(ns string) {
		if ns == "" {
			return
		}
		if _, ok := seen[ns]; ok {
			return
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	if profile.Spec.Requires != nil && profile.Spec.Requires.Services != nil && profile.Spec.Requires.Services.Database != nil {
		add(r.componentDatabaseNamespace(tenant))
	}
	for i := range profile.Spec.Expose {
		if profile.Spec.Expose[i].ForwardToken {
			add(layout.Namespace(layout.Edge))
			break
		}
	}
	// A component that asked where the director is intends to call it, and the
	// director lives in the control namespace. Following the mapping rather
	// than a name means the console and the desktop both reach it and a
	// component that never asked cannot.
	// Both the director and the custodian are in the control
	// namespace, and naming either is what opens the way there.
	if wantsDirector(profile) || wantsCustodian(profile) {
		add(layout.Namespace(layout.Control))
	}
	// The usher is in the edge namespace.
	if wantsUsher(profile) {
		add(layout.Namespace(layout.Edge))
	}
	return out
}

// buildComponentNetworkPolicy is the policy for one component: its pods, by
// the release label the chart puts on them, may leave for the namespaces
// given. Ports are the peer namespace's business; what is granted is the
// function, as the baseline grants the edge.
func buildComponentNetworkPolicy(comp *gentianov1alpha1.Component, egressNamespaces []string, granted []networkingv1.NetworkPolicyEgressRule) *networkingv1.NetworkPolicy {
	egress := make([]networkingv1.NetworkPolicyEgressRule, 0, len(egressNamespaces)+len(granted))
	for _, ns := range egressNamespaces {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
				},
			}},
		})
	}
	// Then the privileges somebody granted. They come last so the rules the
	// requirements imply are readable at the top of a policy a person is
	// reading to find out why an app can reach the internet: everything below
	// the namespace selectors was approved by name.
	egress = append(egress, granted...)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      componentNetworkPolicyName(comp),
			Namespace: comp.Namespace,
			Labels:    componentLabels(comp),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{componentInstanceLabel: releaseName(comp)},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// ensureNetworkPolicy keeps the component's egress policy, or removes it
// when the component may reach nothing beyond the baseline.
func (r *ComponentReconciler) ensureNetworkPolicy(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) error {
	desired := buildComponentNetworkPolicy(comp, r.componentEgressNamespaces(profile, tenant),
		security.GrantedEgressRules(profile, comp, time.Now()))
	key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, key, existing)
	if len(desired.Spec.Egress) == 0 {
		if err == nil {
			return client.IgnoreNotFound(r.Delete(ctx, existing))
		}
		return client.IgnoreNotFound(err)
	}
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return err
	}
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create NetworkPolicy %s: %w", desired.Name, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) && equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	if err := r.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("patch NetworkPolicy %s: %w", desired.Name, err)
	}
	return nil
}
