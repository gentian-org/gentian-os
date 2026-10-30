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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
func (r *ComponentReconciler) componentEgressNamespaces(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant) []string {
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
	// The database, where this reconciler fulfilled it: a placed
	// component's. An installed app's way there is the tenant's
	// (kernel-access-<app>), on the database's port only.
	if operatorPlaced(profile) && profile.Spec.Requires != nil && profile.Spec.Requires.Services != nil && profile.Spec.Requires.Services.Database != nil {
		add(r.componentDatabaseNamespace(tenant, profile))
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
	// The director, the custodian, the registrar and the usher are all in
	// the control namespace, and naming any of them is what opens the way
	// there.
	// A component that vouches for people reaches the registrar as well,
	// whether or not its chart takes the address as a value: that is where a
	// person is linked to it, and where it takes a link away.
	// So does one that commissions partners, at the director.
	if wantsDirector(profile) || wantsCustodian(profile) || wantsRegistrar(profile) || wantsUsher(profile) ||
		wantsVouching(profile) || wantsPartnerCommission(profile) {
		add(layout.Namespace(layout.Control))
	}
	// And the realm, which it posts its statements to; a partner app posts
	// its exchanges and grants there.
	if wantsVouching(profile) || wantsPartners(profile) {
		add(layout.Namespace(layout.Authentication))
	}
	return out
}

// releasePods selects the pods of one Helm release by the label a chart puts
// on them.
func releasePods(release string) map[string]string {
	return map[string]string{componentInstanceLabel: release}
}

// componentPods selects a component's pods. A chart the platform places is
// selected by its release. Any other chart is selected by the app label it is
// rendered with on every pod (apprender), as the tenant's policies for it
// (kernel-access-<app>, app-internal-<app>) are: not every chart sets the
// release label.
func componentPods(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) map[string]string {
	if operatorPlaced(profile) {
		return releasePods(helmReleaseOf(comp, profile))
	}
	return map[string]string{appLabel: comp.Name}
}

// buildComponentNetworkPolicy is the policy for one component: its pods
// (componentPods, which differs for a chart the platform places) may leave for the
// namespaces given. Ports are the peer namespace's business; what is granted
// is the function, as the baseline grants the edge.
func buildComponentNetworkPolicy(comp *gentianov1alpha1.Component, pods map[string]string, egressNamespaces []string, granted []networkingv1.NetworkPolicyEgressRule) *networkingv1.NetworkPolicy {
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
				MatchLabels: pods,
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// legacyGrantedEgressPolicyName is the policy that once held, for an app the
// Composition installs, the rules a person granted on it. Those rules are in
// the component's own policy now, as for any other component, and this name
// is only ever removed.
func legacyGrantedEgressPolicyName(comp *gentianov1alpha1.Component) string {
	return "granted-egress-" + comp.Name
}

// ensureNetworkPolicy keeps the component's egress policy, or removes it
// when the component may reach nothing beyond the baseline.
//
// Whoever renders the chart: the tenant's policies for an app
// (app-internal-<app>, kernel-access-<app>) follow from the requirements it
// declares and know nothing of what this reconciler wires -- the realm for a
// partner app, the control namespace, the rights check, granted privileges --
// so an app without this policy reached none of them.
//
// modelGateway says the component's key at the model gateway is delivered
// (modelAccessFor): only then is the gateway's port opened to it.
func (r *ComponentReconciler) ensureNetworkPolicy(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant, modelGateway bool) error {
	// The way to a store outside the cluster, for a profile that asked where
	// the store is and while the cluster offers one. Ahead of the granted
	// privileges: it follows from the profile's mapping, as the namespaces
	// do, and is nothing a person approved.
	outside, err := r.storeEgress(ctx, profile)
	if err != nil {
		return fmt.Errorf("read whether the cluster offers an App Store: %w", err)
	}
	// The model gateway, for a placed component that declared it and holds
	// its key: one port of one namespace. It follows from the requirement,
	// like the store, and is ahead of the granted privileges for the same
	// reason.
	if modelGateway {
		outside = append(outside, modelGatewayEgress())
	}
	// The bouncer's listener for the rights check, for a component that
	// declared it: one port of the edge namespace.
	if wantsRightsCheck(profile) {
		outside = append(outside, rightsCheckEgress())
	}
	// The arbiter, for a component that declared an authorization store:
	// its pods on its one port.
	if wantsAuthorization(profile) {
		outside = append(outside, authorizationEgress())
	}
	rules := append(outside, security.GrantedEgressRules(profile, comp, time.Now())...)
	// The cluster's own public address, where a rule to public addresses
	// admits it -- the store's way out, or a granted one: from inside it is
	// the edge's pods, which those rules do not name (own_public_address.go).
	//
	// An edge that cannot be read leaves the rule out and the rest of the
	// policy as it should be: the rule only adds a way, so leaving it out
	// fails closed, while refusing the whole policy would also hold back a
	// privilege somebody withdrew.
	own, err := r.ownPublicAddressRule(ctx, rules)
	if err != nil {
		logf.FromContext(ctx).Info("the edge's public address could not be read; the policy leaves it out",
			"component", comp.Name, "error", err.Error())
		own = nil
	}
	// The policy that held a composed app's granted rules on their own; they
	// are in this one now.
	legacy := &networkingv1.NetworkPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: legacyGrantedEgressPolicyName(comp), Namespace: comp.Namespace}, legacy); err == nil {
		if err := r.Delete(ctx, legacy); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete NetworkPolicy %s: %w", legacy.Name, err)
		}
	} else if !errors.IsNotFound(err) {
		return err
	}
	desired := buildComponentNetworkPolicy(comp, componentPods(comp, profile), r.componentEgressNamespaces(comp, profile, tenant),
		append(rules, own...))
	key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
	existing := &networkingv1.NetworkPolicy{}
	err = r.Get(ctx, key, existing)
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
