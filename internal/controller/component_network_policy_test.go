/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"slices"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A tenant namespace is closed; a component reaches what its requirements
// were fulfilled with. The desktop of the platform tenant reaches the
// kernel's postgres and the director, and nothing else.
func TestTheDesktopReachesItsDatabaseAndTheDirector(t *testing.T) {
	// The v5 layout, so that the three functions are three namespaces; in
	// the v4 fallback data and edge are one, and the list would dedupe.
	t.Setenv("GENTIAN_NS_DATA", "kernel-data")
	t.Setenv("GENTIAN_NS_EDGE", "kernel-edge")
	t.Setenv("GENTIAN_NS_CONTROL", "kernel-control")
	r := &ComponentReconciler{KernelRealm: "kernel"}
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "platform"
	tenant.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Name = DesktopProfileName
	profile.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementEvery}
	// What the shipped desktop profile declares: it asks where the director
	// is, and that request is what opens its egress to the control namespace.
	// Nothing about the name "desktop" does.
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		Platform: &gentianov1alpha1.PlatformValueMapping{DirectorURLKey: "director.url"},
	}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
		Services: &gentianov1alpha1.ServiceRequirements{Database: &gentianov1alpha1.DatabaseRequirement{}},
	}
	profile.Spec.Expose = []gentianov1alpha1.ExposureSpec{{Name: "api", Surface: gentianov1alpha1.SurfaceGateway, ForwardToken: true}}
	comp := &gentianov1alpha1.Component{}
	comp.Name = "desktop"
	comp.Namespace = "tenant-platform"

	got := r.componentEgressNamespaces(nil, profile, tenant)
	// Its database, the edge it verifies forwarded tokens through, the director.
	want := []string{layout.Namespace(layout.Data), layout.Namespace(layout.Edge), layout.Namespace(layout.Control)}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("egress namespaces = %v, want %v", got, want)
	}

	np := buildComponentNetworkPolicy(comp, releasePods(releaseName(comp)), got, nil)
	if np.Name != "component-desktop" || np.Namespace != "tenant-platform" {
		t.Fatalf("policy named %s/%s", np.Namespace, np.Name)
	}
	if sel := np.Spec.PodSelector.MatchLabels[componentInstanceLabel]; sel != "tenant-platform-desktop" {
		t.Fatalf("pods selected by instance %q, want the release name", sel)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("policy types = %v, want egress only", np.Spec.PolicyTypes)
	}
	for i, rule := range np.Spec.Egress {
		if len(rule.To) != 1 || rule.To[0].NamespaceSelector == nil {
			t.Fatalf("egress rule %d = %+v", i, rule)
		}
		if ns := rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; ns != want[i] {
			t.Fatalf("egress rule %d reaches %q, want %q", i, ns, want[i])
		}
	}
	if len(np.Labels) == 0 || np.Labels[componentLabel] != "desktop" {
		t.Fatalf("labels = %v", np.Labels)
	}
}

// Another tenant's desktop reaches the tenant postgres of the system tier,
// not the kernel's data plane; with no token forwarded to it, not the edge.
func TestATenantDesktopReachesTheSystemPostgres(t *testing.T) {
	t.Parallel()
	r := &ComponentReconciler{KernelRealm: "kernel"}
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Name = DesktopProfileName
	// What the shipped desktop profile declares: it asks where the director
	// is, and that request is what opens its egress to the control namespace.
	// Nothing about the name "desktop" does.
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		Platform: &gentianov1alpha1.PlatformValueMapping{DirectorURLKey: "director.url"},
	}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{
		Services: &gentianov1alpha1.ServiceRequirements{Database: &gentianov1alpha1.DatabaseRequirement{}},
	}
	// Placed, as the shipped desktop is: a placed component's database is
	// the operator's to open. An installed app's way there is the tenant's.
	profile.Spec.Placement = &gentianov1alpha1.ComponentPlacement{Tenants: gentianov1alpha1.PlacementEvery}
	got := r.componentEgressNamespaces(nil, profile, tenant)
	if len(got) != 2 || got[0] != postgresNamespace || got[1] != layout.Namespace(layout.Control) {
		t.Fatalf("egress namespaces = %v", got)
	}
}

// The usher runs in the control namespace. A component that asks where it is
// is let through to that namespace, as one that asks for the director is, and
// is not let through to the edge on that account.
func TestAskingForTheUsherOpensTheControlNamespace(t *testing.T) {
	t.Setenv("GENTIAN_NS_EDGE", "kernel-edge")
	t.Setenv("GENTIAN_NS_CONTROL", "kernel-control")
	r := &ComponentReconciler{KernelRealm: "kernel"}
	tenant := &gentianov1alpha1.Tenant{}
	tenant.Name = "demo"
	profile := &gentianov1alpha1.ComponentProfile{}
	profile.Spec.Package.ValueMapping = &gentianov1alpha1.ValueMapping{
		Platform: &gentianov1alpha1.PlatformValueMapping{UsherURLKey: "usher.url"},
	}
	got := r.componentEgressNamespaces(nil, profile, tenant)
	if len(got) != 1 || got[0] != "kernel-control" {
		t.Fatalf("egress namespaces = %v, want only the control namespace", got)
	}
	if want := "http://gentian-os-usher.kernel-control.svc.cluster.local:8080"; r.usherURL() != want {
		t.Fatalf("the usher's address is %s, want %s", r.usherURL(), want)
	}
}

// An exchanged token is verified against the issuer as a forwarded one is, and
// the issuer is reached through the edge. An app that is handed no token has
// no reason to go there.
func TestAComponentHandedATokenReachesTheEdge(t *testing.T) {
	r := &ComponentReconciler{}
	for name, tc := range map[string]struct {
		expose gentianov1alpha1.ExposureSpec
		want   bool
	}{
		"forwarded": {gentianov1alpha1.ExposureSpec{Name: "api", ForwardToken: true}, true},
		"exchanged": {gentianov1alpha1.ExposureSpec{Name: "api", ExchangeToken: true}, true},
		"none":      {gentianov1alpha1.ExposureSpec{Name: "web"}, false},
	} {
		profile := &gentianov1alpha1.ComponentProfile{}
		profile.Spec.Expose = []gentianov1alpha1.ExposureSpec{tc.expose}
		got := r.componentEgressNamespaces(nil, profile, nil)
		if reaches := slices.Contains(got, layout.Namespace(layout.Edge)); reaches != tc.want {
			t.Errorf("%s: egress namespaces = %v, reaches the edge = %v, want %v", name, got, reaches, tc.want)
		}
	}
}
