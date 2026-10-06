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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A profile pinned to a new chart version reaches the Release through the
// components of that profile, and only those.
func TestAProfileChangeRunsItsComponents(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	desktopA := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "tenant-platform"}}
	desktopA.Spec.ProfileRef.Name = "desktop"
	desktopB := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: "tenant-demo"}}
	desktopB.Spec.ProfileRef.Name = "desktop"
	other := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "tenant-demo"}}
	other.Spec.ProfileRef.Name = "wiki"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(desktopA, desktopB, other).Build()

	got := componentsReferencingProfile(context.Background(), c, "desktop")
	if len(got) != 2 {
		t.Fatalf("requests = %v, want the two desktops", got)
	}
	for _, req := range got {
		if req.Name != "desktop" {
			t.Fatalf("request %v is not a desktop", req)
		}
	}
	// An addon's profile runs the component that activates it as well as its
	// own: a base held for a pinned addon is released by the addon's profile
	// becoming the pinned build.
	base := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "odoo", Namespace: "tenant-demo"}}
	base.Spec.ProfileRef.Name = "odoo"
	base.Spec.Addons = []string{"odoo-crm"}
	addon := &gentianov1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Name: "odoo-crm", Namespace: "tenant-demo"}}
	addon.Spec.ProfileRef.Name = "odoo-crm"
	for _, obj := range []*gentianov1alpha1.Component{base, addon} {
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
	}
	if got := componentsReferencingProfile(context.Background(), c, "odoo-crm"); len(got) != 2 {
		t.Fatalf("requests = %v, want the addon and its base", got)
	}
	if got := componentsReferencingProfile(context.Background(), c, "nothing"); len(got) != 0 {
		t.Fatalf("a profile nothing references runs %v", got)
	}
}
