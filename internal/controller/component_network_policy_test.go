/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
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

	got := r.componentEgressNamespaces(profile, tenant)
	// Its database, the edge it verifies forwarded tokens through, the director.
	want := []string{layout.Namespace(layout.Data), layout.Namespace(layout.Edge), layout.Namespace(layout.Control)}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("egress namespaces = %v, want %v", got, want)
	}

	np := buildComponentNetworkPolicy(comp, got)
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
	got := r.componentEgressNamespaces(profile, tenant)
	if len(got) != 2 || got[0] != postgresNamespace || got[1] != layout.Namespace(layout.Control) {
		t.Fatalf("egress namespaces = %v", got)
	}
}
