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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A component that asked where the desktop's API is (valueMapping.platform
// .desktopUrlKey) calls it in-cluster, with the person's own token: the App
// Store app storing a tile the tenant chose. A tenant's namespace is closed
// between its own pods (netpolicy.BaselineNetworkPolicy), so asking is what
// opens the way, and it opens two things and nothing more: the component's
// pods may leave for the desktop's pods on the API's port, and the desktop's
// pods admit the component's on it. Both policies are the component's, so
// they go with it.

// desktopAPIEntry is the desktop profile's entry for its API.
const desktopAPIEntry = "api"

// wantsDesktop reports whether a profile asked to be told where the desktop's
// API is. Only a platform-trust profile may; the schema says so as well.
func wantsDesktop(profile *gentianov1alpha1.ComponentProfile) bool {
	return platformMapping(profile) != nil && platformMapping(profile).DesktopURLKey != "" &&
		profile.Spec.TrustTier == gentianov1alpha1.TrustTierPlatform
}

// desktopAPI is the desktop's API in one namespace: the Service and port of
// the desktop profile's "api" entry, and the release its pods belong to.
type desktopAPI struct {
	service string
	port    int32
	release string
}

// url is the address a component in the same namespace calls.
func (d desktopAPI) url() string {
	return fmt.Sprintf("http://%s:%d", d.service, d.port)
}

// desktopAPIIn reads where the desktop's API is in a namespace, from the
// profile the desktop is installed from. False when the cluster has no
// desktop profile or the profile has no API entry of its own.
func (r *ComponentReconciler) desktopAPIIn(ctx context.Context, namespace string) (desktopAPI, bool, error) {
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: DesktopProfileName}, profile); err != nil {
		if errors.IsNotFound(err) {
			return desktopAPI{}, false, nil
		}
		return desktopAPI{}, false, err
	}
	for _, e := range profile.Spec.Expose {
		if e.Name != desktopAPIEntry || e.Backend.Component != "" || e.Backend.Service == "" || e.Backend.Port == 0 {
			continue
		}
		desktop := &gentianov1alpha1.Component{}
		desktop.Name, desktop.Namespace = DesktopComponentName, namespace
		return desktopAPI{service: e.Backend.Service, port: e.Backend.Port, release: helmReleaseOf(desktop, profile)}, true, nil
	}
	return desktopAPI{}, false, nil
}

// desktopURL is what lands under desktopUrlKey: the desktop's API address,
// or the empty string where the profile may not ask or there is no desktop.
func (r *ComponentReconciler) desktopURL(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) (string, error) {
	if !wantsDesktop(profile) {
		return "", nil
	}
	api, ok, err := r.desktopAPIIn(ctx, comp.Namespace)
	if err != nil || !ok {
		return "", err
	}
	return api.url(), nil
}

func desktopAccessEgressName(comp *gentianov1alpha1.Component) string {
	return policyName("desktop-api-from-" + comp.Name)
}

func desktopAccessIngressName(comp *gentianov1alpha1.Component) string {
	return policyName("desktop-api-admits-" + comp.Name)
}

// policyName keeps a name within what an object's name may be.
func policyName(name string) string {
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// buildDesktopAccessPolicies are the two policies of one component's way to
// the desktop's API. The port is the Service's, which is the API container's
// own: a policy matches the port a pod listens on, and the desktop chart
// serves its API on the port its Service names.
func buildDesktopAccessPolicies(comp *gentianov1alpha1.Component, componentRelease string, api desktopAPI) []*networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(api.port)
	ports := []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}
	componentPods := metav1.LabelSelector{MatchLabels: map[string]string{componentInstanceLabel: componentRelease}}
	desktopPods := metav1.LabelSelector{MatchLabels: map[string]string{componentInstanceLabel: api.release}}
	return []*networkingv1.NetworkPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Name: desktopAccessEgressName(comp), Namespace: comp.Namespace, Labels: componentLabels(comp)},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: componentPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &desktopPods}},
					Ports: ports,
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: desktopAccessIngressName(comp), Namespace: comp.Namespace, Labels: componentLabels(comp)},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: desktopPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &componentPods}},
					Ports: ports,
				}},
			},
		},
	}
}

// ensureDesktopAccess keeps a component's way to the desktop's API, or
// removes it when the component did not ask, may not, or there is no desktop.
// Whichever way the component is delivered: the pods are selected by the
// release they belong to (helmReleaseOf).
func (r *ComponentReconciler) ensureDesktopAccess(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) error {
	var desired []*networkingv1.NetworkPolicy
	if wantsDesktop(profile) {
		api, ok, err := r.desktopAPIIn(ctx, comp.Namespace)
		if err != nil {
			return fmt.Errorf("read where the desktop's API is: %w", err)
		}
		if ok {
			desired = buildDesktopAccessPolicies(comp, helmReleaseOf(comp, profile), api)
		}
	}
	if len(desired) == 0 {
		for _, name := range []string{desktopAccessEgressName(comp), desktopAccessIngressName(comp)} {
			np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: comp.Namespace}}
			if err := r.Delete(ctx, np); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("delete NetworkPolicy %s: %w", name, err)
			}
		}
		return nil
	}
	for _, np := range desired {
		if err := r.applyComponentPolicy(ctx, comp, np); err != nil {
			return err
		}
	}
	return nil
}

// applyComponentPolicy creates or updates one NetworkPolicy the component
// owns.
func (r *ComponentReconciler) applyComponentPolicy(ctx context.Context, comp *gentianov1alpha1.Component, desired *networkingv1.NetworkPolicy) error {
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return err
	}
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
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
	existing.Spec, existing.Labels = desired.Spec, desired.Labels
	if err := r.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("patch NetworkPolicy %s: %w", desired.Name, err)
	}
	return nil
}
