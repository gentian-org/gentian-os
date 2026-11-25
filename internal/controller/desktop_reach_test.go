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
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func shippedLiaisonProfile(t *testing.T) *gentianov1alpha1.ComponentProfile {
	return renderShippedProfile(t, "componentprofile-liaison.yaml",
		"liaison.enabled=true", "liaison.chart.version=0.1.0-develop.0000000")
}

// The desktop reaches the liaison in their namespace, and only that: the
// desktop's pods may leave for the liaison's pods on the port of the
// liaison's gateway entry, and the liaison's pods admit the desktop's on it.
// The liaison is told where the desktop answers and where it answers itself,
// and the edge audience the desktop's token carries. A profile of another
// tier that named the key gets neither the policies nor the host.
func TestTheDesktopReachesTheLiaisonAndTheLiaisonKnowsWhereTheDesktopIs(t *testing.T) {
	scheme := appStoreScheme(t)
	ctx := context.Background()
	profile := shippedLiaisonProfile(t)
	comp := tileFixtureComponent("liaison", "tenant-acme", "liaison")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(shippedDesktopProfile(t), comp.DeepCopy()).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	live := &gentianov1alpha1.Component{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(comp), live); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureDesktopReach(ctx, live, defaultTenantName(live), profile); err != nil {
		t.Fatal(err)
	}
	get := func(name string) *networkingv1.NetworkPolicy {
		np := &networkingv1.NetworkPolicy{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "tenant-acme"}, np); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(np.OwnerReferences) != 1 || np.OwnerReferences[0].Name != "liaison" {
			t.Fatalf("%s is not the liaison's: %+v", name, np.OwnerReferences)
		}
		return np
	}
	liaisonPods := map[string]string{componentInstanceLabel: "tenant-acme-liaison"}
	desktopPods := map[string]string{componentInstanceLabel: "tenant-acme-desktop"}
	onePort := func(name string, ports []networkingv1.NetworkPolicyPort) {
		if len(ports) != 1 || ports[0].Port.IntValue() != 8000 || *ports[0].Protocol != corev1.ProtocolTCP {
			t.Fatalf("%s: ports = %+v, want TCP 8000 only", name, ports)
		}
	}

	out := get("desktop-to-liaison")
	if fmt.Sprint(out.Spec.PodSelector.MatchLabels) != fmt.Sprint(desktopPods) ||
		fmt.Sprint(out.Spec.PolicyTypes) != "[Egress]" || len(out.Spec.Egress) != 1 {
		t.Fatalf("egress policy = %+v", out.Spec)
	}
	to := out.Spec.Egress[0].To
	if len(to) != 1 || to[0].NamespaceSelector != nil || to[0].PodSelector == nil ||
		fmt.Sprint(to[0].PodSelector.MatchLabels) != fmt.Sprint(liaisonPods) {
		t.Fatalf("egress peer = %+v, want the liaison's pods in the same namespace", to)
	}
	onePort("egress", out.Spec.Egress[0].Ports)

	in := get("liaison-admits-desktop")
	if fmt.Sprint(in.Spec.PodSelector.MatchLabels) != fmt.Sprint(liaisonPods) ||
		fmt.Sprint(in.Spec.PolicyTypes) != "[Ingress]" || len(in.Spec.Ingress) != 1 {
		t.Fatalf("ingress policy = %+v", in.Spec)
	}
	from := in.Spec.Ingress[0].From
	if len(from) != 1 || from[0].NamespaceSelector != nil || from[0].PodSelector == nil ||
		fmt.Sprint(from[0].PodSelector.MatchLabels) != fmt.Sprint(desktopPods) {
		t.Fatalf("ingress peer = %+v, want the desktop's pods in the same namespace", from)
	}
	onePort("ingress", in.Spec.Ingress[0].Ports)

	zone := edgeZone{zoneNames: zoneNames{domain: "acme.k.example"}}
	values, err := r.placementValues(ctx, live, profile, zone)
	if err != nil {
		t.Fatal(err)
	}
	if values["desktopHost"] != "desktop.acme.k.example" || values["host"] != "liaison.acme.k.example" {
		t.Fatalf("placement values = %+v", values)
	}
	if m := profile.Spec.Package.ValueMapping.Platform; m.AudienceKey != "auth.edgeAudience" {
		t.Fatalf("the liaison is not told the edge audience: %+v", m)
	}

	other := profile.DeepCopy()
	other.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	if err := r.ensureDesktopReach(ctx, live, defaultTenantName(live), other); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"desktop-to-liaison", "liaison-admits-desktop"} {
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "tenant-acme"}, &networkingv1.NetworkPolicy{}); err == nil {
			t.Errorf("%s is still there for a certified profile", name)
		}
	}
	if host, err := r.desktopHost(ctx, other, zone.zoneNames); err != nil || host != "" {
		t.Fatalf("a certified profile was told the desktop: %q (%v)", host, err)
	}
}
