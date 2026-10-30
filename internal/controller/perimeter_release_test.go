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
	"path/filepath"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// perimeterPolicySelectors publishes the shares entry of profile for a
// component with the given annotations and returns the pods its two policies
// name: those the proxy may reach, and those that take the proxy's traffic.
func perimeterPolicySelectors(t *testing.T, profile *gentianov1alpha1.ComponentProfile, annotations map[string]string) (egress, ingress string) {
	t.Helper()
	scheme := perimeterScheme()
	tenant := acmeTenantFixture()
	comp := componentFor(profile.Name, tenantNamespaceName(tenant))
	comp.Annotations = annotations
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
// installed has one release name, and an app the Composition installed and
// the operator took over keeps the other; a policy written with the wrong
// one selects no pod, and the surface answers 504 with its route, its
// listener and its proxy all in place.
func TestThePerimeterReachesThePodsOfTheAppsOwnRelease(t *testing.T) {
	profile := nextcloudWithPerimeter()
	egress, ingress := perimeterPolicySelectors(t, profile, nil)
	if want := "tenant-acme-" + profile.Name; egress != want || ingress != want {
		t.Fatalf("an app the operator installed: egress to %q, ingress on %q, want %q", egress, ingress, want)
	}

	// The same app taken over from the composite the Composition installed
	// it as.
	egress, ingress = perimeterPolicySelectors(t, nextcloudWithPerimeter(), map[string]string{compositeAnnotation: "tenant-acme-nextcloud-x1"})
	if want := profile.Name + composedReleaseSuffix; egress != want || ingress != want {
		t.Fatalf("an app taken over from the Composition: egress to %q, ingress on %q, want %q", egress, ingress, want)
	}
}

// An app taken over from its composite keeps every name the Composition gave
// it: the Release object, the Helm release it installs, and the keep rule it
// reads. Those are what renderNames gives the renderer, and the Composition's
// own render (its fixture) is what they have to match; a release under
// another name would be a second install beside the first.
func TestATakenOverAppKeepsTheCompositionsReleaseNames(t *testing.T) {
	var release *unstructured.Unstructured
	for _, obj := range yamlDocs(t, filepath.Join(apprenderFixtureRoot, "app-default", "expected.yaml")) {
		if obj.GetKind() == "Release" {
			release = obj
		}
	}
	if release == nil {
		t.Fatal("the fixture renders no Release")
	}
	xr := yamlDocs(t, filepath.Join(apprenderFixtureRoot, "app-default", "xr.yaml"))[0]
	app, _, _ := unstructured.NestedString(xr.Object, "spec", "claimRef", "name")
	comp := componentFor(app, "tenant-demo")
	comp.Annotations = map[string]string{compositeAnnotation: xr.GetName()}

	object, helm, keep, composite := renderNames(comp)
	if object != release.GetName() || composite != xr.GetName() {
		t.Errorf("Release object %q of composite %q, the Composition's %q of %q", object, composite, release.GetName(), xr.GetName())
	}
	if ext := release.GetAnnotations()["crossplane.io/external-name"]; helm != ext || helmReleaseOf(comp, chartProfileFixture("oci://x")) != ext {
		t.Errorf("Helm release %q, the Composition's %q", helm, ext)
	}
	patches, _, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "patchesFrom")
	if len(patches) != 1 {
		t.Fatalf("patchesFrom = %v", patches)
	}
	if name, _, _ := unstructured.NestedString(patches[0].(map[string]interface{}), "configMapKeyRef", "name"); name != keep {
		t.Errorf("keep rule %q, the Composition's %q", keep, name)
	}
}
