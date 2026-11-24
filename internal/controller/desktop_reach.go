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
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A component that asked where the desktop answers (valueMapping.platform
// .desktopHostKey) is one the desktop calls: the liaison, whose connections a
// person manages in the desktop's account panel. It is the way of
// desktop_access.go turned around, and opens two things and nothing more: the
// desktop's pods may leave for the component's pods on the ports of the
// component's own gateway entries, and the component's pods admit the
// desktop's on them. Both policies are the component's, so they go with it.

// wantsDesktopReach reports whether a profile asked to be told where the
// desktop answers. Only a platform-trust profile may; the schema says so as
// well.
func wantsDesktopReach(profile *gentianov1alpha1.ComponentProfile) bool {
	return platformMapping(profile) != nil && platformMapping(profile).DesktopHostKey != "" &&
		profile.Spec.TrustTier == gentianov1alpha1.TrustTierPlatform
}

// desktopHost is what lands under desktopHostKey: the host of the desktop
// profile's first gateway entry in the component's zone, or the empty string
// where the profile may not ask or there is no desktop.
func (r *ComponentReconciler) desktopHost(ctx context.Context, profile *gentianov1alpha1.ComponentProfile, zone zoneNames) (string, error) {
	if !wantsDesktopReach(profile) {
		return "", nil
	}
	desktop := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: DesktopProfileName}, desktop); err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	for i := range desktop.Spec.Expose {
		e := &desktop.Spec.Expose[i]
		if e.Surface != gentianov1alpha1.SurfaceGateway || (e.Backend.Component != "" && e.Backend.Component != DesktopComponentName) {
			continue
		}
		return exposureHostIn(zone, DesktopComponentName, e), nil
	}
	return "", nil
}

// ownGatewayPorts are the ports the component's own gateway entries reach it
// on, each once.
func ownGatewayPorts(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) []int32 {
	seen := map[int32]bool{}
	var ports []int32
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.Surface != gentianov1alpha1.SurfaceGateway || e.Backend.Port == 0 ||
			(e.Backend.Component != "" && e.Backend.Component != comp.Name) || seen[e.Backend.Port] {
			continue
		}
		seen[e.Backend.Port] = true
		ports = append(ports, e.Backend.Port)
	}
	sort.Slice(ports, func(a, b int) bool { return ports[a] < ports[b] })
	return ports
}

func desktopReachEgressName(comp *gentianov1alpha1.Component) string {
	return policyName("desktop-to-" + comp.Name)
}

func desktopReachIngressName(comp *gentianov1alpha1.Component) string {
	return policyName(comp.Name + "-admits-desktop")
}

// buildDesktopReachPolicies are the two policies of the desktop's way to one
// component. The ports are the component's Services', which are its
// containers' own, as in buildDesktopAccessPolicies.
func buildDesktopReachPolicies(comp *gentianov1alpha1.Component, tenantName, componentRelease, desktopRelease string, ports []int32) []*networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	var allowed []networkingv1.NetworkPolicyPort
	for _, p := range ports {
		port := intstr.FromInt32(p)
		allowed = append(allowed, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	componentPods := metav1.LabelSelector{MatchLabels: map[string]string{componentInstanceLabel: componentRelease}}
	desktopPods := metav1.LabelSelector{MatchLabels: map[string]string{componentInstanceLabel: desktopRelease}}
	return []*networkingv1.NetworkPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Name: desktopReachEgressName(comp), Namespace: comp.Namespace, Labels: componentLabels(comp, tenantName)},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: desktopPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &componentPods}},
					Ports: allowed,
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: desktopReachIngressName(comp), Namespace: comp.Namespace, Labels: componentLabels(comp, tenantName)},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: componentPods,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &desktopPods}},
					Ports: allowed,
				}},
			},
		},
	}
}

// ensureDesktopReach keeps the desktop's way to a component, or removes it
// when the component did not ask, may not, has no gateway entry of its own,
// or there is no desktop.
func (r *ComponentReconciler) ensureDesktopReach(ctx context.Context, comp *gentianov1alpha1.Component, tenantName string, profile *gentianov1alpha1.ComponentProfile) error {
	var desired []*networkingv1.NetworkPolicy
	if wantsDesktopReach(profile) {
		if ports := ownGatewayPorts(comp, profile); len(ports) > 0 {
			api, ok, err := r.desktopAPIIn(ctx, comp.Namespace)
			if err != nil {
				return fmt.Errorf("read where the desktop is: %w", err)
			}
			if ok {
				desired = buildDesktopReachPolicies(comp, tenantName, helmReleaseOf(comp, profile), api.release, ports)
			}
		}
	}
	if len(desired) == 0 {
		for _, name := range []string{desktopReachEgressName(comp), desktopReachIngressName(comp)} {
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
