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
	"os"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// perimeterPolicySelectors publishes the shares entry of profile and returns
// the pods its two policies name: those the proxy may reach, and those that
// take the proxy's traffic.
func perimeterPolicySelectors(t *testing.T, profile *gentianov1alpha1.ComponentProfile) (egress, ingress string) {
	t.Helper()
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	comp := componentFor(profile.Name, tenantNamespaceName(tenant))
	comp.Spec.Exposures = enabledShares(time.Now().Add(720 * time.Hour))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant.DeepCopy(), profile, comp).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example"}
	ctx := context.Background()
	if n, err := r.ensurePerimeter(ctx, comp, profile, tenant, r.zoneOf(tenant)); err != nil || n != 1 {
		t.Fatalf("published %d, %v", n, err)
	}
	name := perimeterName(comp, "shares")
	out := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: name + "-egress", Namespace: layout.TenantDMZ(tenant.Name)}, out); err != nil {
		t.Fatal(err)
	}
	in := &networkingv1.NetworkPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: name + "-ingress", Namespace: comp.Namespace}, in); err != nil {
		t.Fatal(err)
	}
	return out.Spec.Egress[0].To[0].PodSelector.MatchLabels[componentInstanceLabel],
		in.Spec.PodSelector.MatchLabels[componentInstanceLabel]
}

// The proxy of a published surface reaches the pods of the app behind it, and
// the pods of an app are those of its Helm release. An app the operator
// installs itself has one release name and an app the Composition installs
// another; a policy written with the wrong one selects no pod, and the
// surface answers 504 with its route, its listener and its proxy all in
// place.
func TestThePerimeterReachesThePodsOfTheAppsOwnRelease(t *testing.T) {
	direct := nextcloudWithPerimeter()
	if composedDelivery(direct) {
		t.Fatal("the fixture is meant to be an app the operator installs itself")
	}
	egress, ingress := perimeterPolicySelectors(t, direct)
	if want := "tenant-acme-" + direct.Name; egress != want || ingress != want {
		t.Fatalf("an app the operator installs: egress to %q, ingress on %q, want %q", egress, ingress, want)
	}

	// The same app asking for something only the Composition provides: a
	// sign-in client of its own.
	composed := nextcloudWithPerimeter()
	composed.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		OIDC: &gentianov1alpha1.OIDCValueMapping{IssuerKey: "oidc.issuer"},
	}
	if !composedDelivery(composed) {
		t.Fatal("the fixture is meant to be an app the Composition installs")
	}
	egress, ingress = perimeterPolicySelectors(t, composed)
	if want := composed.Name + "-release"; egress != want || ingress != want {
		t.Fatalf("an app the Composition installs: egress to %q, ingress on %q, want %q", egress, ingress, want)
	}
}

// The name is the Composition's to choose and is written there, not here. If
// it is changed there, this is where it has to follow.
func TestTheComposedReleaseNameIsTheCompositions(t *testing.T) {
	raw, err := os.ReadFile("../../crossplane/compositions/app-default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{{- $release := printf "%s` + composedReleaseSuffix + `" $app }}`; !strings.Contains(string(raw), want) {
		t.Fatalf("the app Composition no longer names its release <app>%s; helmReleaseOf has to name what it does", composedReleaseSuffix)
	}
}
