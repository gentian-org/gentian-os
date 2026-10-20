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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

func reconcileBuild(t *testing.T, build *gentianov1alpha1.ProfileBuild) *gentianov1alpha1.ProfileBuild {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(build).
		WithStatusSubresource(&gentianov1alpha1.ProfileBuild{}).Build()
	r := &ProfileBuildReconciler{Client: c}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(build)}); err != nil {
		t.Fatal(err)
	}
	got := &gentianov1alpha1.ProfileBuild{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(build), got); err != nil {
		t.Fatal(err)
	}
	return got
}

// A build that verifies has its profile's spec on its status, with the
// defaults a ComponentProfile gets, for the Composition to read.
func TestAVerifiedBuildCarriesItsProfilesSpecOnItsStatus(t *testing.T) {
	got := reconcileBuild(t, profileBuildOf(t, wikiBundle))
	verified := meta.FindStatusCondition(got.Status.Conditions, profileBuildVerified)
	if verified == nil || verified.Status != "True" {
		t.Fatalf("Verified = %+v", verified)
	}
	if got.Status.ProfileSpec == nil || got.Status.ProfileSpec.Package.Chart.Version != "1.0.0" {
		t.Fatalf("status.profileSpec = %+v", got.Status.ProfileSpec)
	}
}

// One that does not says so and carries no spec: nothing renders from it.
func TestABuildThatDoesNotVerifyCarriesNoSpec(t *testing.T) {
	build := profileBuildOf(t, wikiBundle)
	build.Spec.Bundle = profilebundle.Encode([]byte(strings.ReplaceAll(wikiBundle, `version: "1.0.0"`, `version: "6.6.6"`)))
	got := reconcileBuild(t, build)
	verified := meta.FindStatusCondition(got.Status.Conditions, profileBuildVerified)
	if verified == nil || verified.Status != "False" || verified.Reason != profilebundle.ReasonMismatch {
		t.Fatalf("Verified = %+v", verified)
	}
	if got.Status.ProfileSpec != nil {
		t.Fatal("a spec was written for a build that is not what it says")
	}
}
