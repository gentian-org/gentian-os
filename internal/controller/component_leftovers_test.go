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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/composed"
)

// leftoverRelease is a Release an earlier composite of the claim composed,
// stuck on an unfinished create the way provider-helm leaves one whose
// install was cut short.
func leftoverRelease(ns, claim, composite string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(helmReleaseGVK)
	u.SetName(composite + "-release")
	u.SetLabels(map[string]string{composed.ClaimNamespaceLabel: ns, composed.ClaimNameLabel: claim, composed.CompositeLabel: composite})
	u.SetAnnotations(map[string]string{"crossplane.io/external-create-pending": "2026-10-26T14:44:10Z"})
	u.SetFinalizers([]string{"finalizer.managedresource.crossplane.io"})
	_ = unstructured.SetNestedField(u.Object, "Delete", "spec", "deletionPolicy")
	return u
}

// A purged app's old Release stayed when its composite went, and the next
// install's Release fought it for the same Helm release. The claim is not
// created beside such a leftover: the leftover is unblocked and deleted
// first, with its own policy, since nothing else manages its release. Once
// it is gone the claim is created; and a leftover that turns up beside a
// live claim is deleted with Orphan, so the release the claim manages stays.
func TestTheAppClaimWaitsForWhatAnEarlierInstallLeftComposed(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	comp := &gentianov1alpha1.Component{}
	comp.Name, comp.Namespace, comp.UID = "wiki", tenantNamespaceName(tenant), "uid-comp"
	comp.Spec.ProfileRef.Name = "wiki"
	old := leftoverRelease(comp.Namespace, comp.Name, "wiki-old")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(old).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}
	zone := edgeZone{zoneNames: zoneNames{domain: "acme.k.example"}}

	claimExists := func() bool {
		claim := &unstructured.Unstructured{}
		claim.SetGroupVersionKind(appClaimGVK)
		return c.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, claim) == nil
	}
	release := func(name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(helmReleaseGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, u); err != nil {
			return nil
		}
		return u
	}

	_, msg, err := r.ensureAppClaim(ctx, comp, tenant, zone, pullSecrets{}, "", "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if claimExists() || !strings.Contains(msg, "wiki-old-release") {
		t.Fatalf("the claim was created beside the leftover: claim=%v msg=%q", claimExists(), msg)
	}
	rel := release("wiki-old-release")
	if rel == nil || rel.GetDeletionTimestamp() == nil {
		t.Fatalf("the leftover was not deleted: %v", rel)
	}
	if _, pending := rel.GetAnnotations()["crossplane.io/external-create-pending"]; pending {
		t.Error("the unfinished create was not unblocked, so the provider never deletes it")
	}
	if policy, _, _ := unstructured.NestedString(rel.Object, "spec", "deletionPolicy"); policy != "Delete" {
		t.Errorf("deletionPolicy = %s: with no claim the leftover takes its release with it", policy)
	}

	// The provider finishes the deletion; the claim follows.
	rel.SetFinalizers(nil)
	if err := c.Update(ctx, rel); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ensureAppClaim(ctx, comp, tenant, zone, pullSecrets{}, "", "", false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !claimExists() {
		t.Fatal("the claim was not created once the leftover had gone")
	}

	// The live claim's composite is wiki-new; another leftover turns up.
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(appClaimGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, claim); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(claim.Object, "wiki-new", "spec", "resourceRef", "name")
	if err := c.Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, leftoverRelease(comp.Namespace, comp.Name, "wiki-older")); err != nil {
		t.Fatal(err)
	}
	ready, msg, err := r.ensureAppClaim(ctx, comp, tenant, zone, pullSecrets{}, "", "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ready || !strings.Contains(msg, "wiki-older-release") {
		t.Fatalf("ready=%v msg=%q", ready, msg)
	}
	rel = release("wiki-older-release")
	if policy, _, _ := unstructured.NestedString(rel.Object, "spec", "deletionPolicy"); policy != "Orphan" || rel.GetDeletionTimestamp() == nil {
		t.Errorf("beside a live claim the leftover must go with Orphan: policy=%s deleting=%v", policy, rel.GetDeletionTimestamp() != nil)
	}
}

// At an uninstall the composite goes and takes what it composed with it,
// except what its provider cannot finish with. The tenant reconciler removes
// that for an app the tenant no longer has; an installed app's leftovers are
// its claim's to deal with (above), and an orphan of an installed app is
// left to it.
func TestAnUninstalledAppsLeftoverCompositionIsRemoved(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	tenant := acmeTenantFixture()
	_ = clientgoscheme.AddToScheme(scheme)
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "drive"}}
	ns := tenantNamespaceName(tenant)
	gone := leftoverRelease(ns, "wiki", "wiki-old")
	installed := leftoverRelease(ns, "drive", "drive-old")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gone, installed).Build()
	r := &TenantReconciler{Client: c, Scheme: scheme}

	if err := r.cleanupOrphanedAppWorkload(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	for name, deleting := range map[string]bool{"wiki-old-release": true, "drive-old-release": false} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(helmReleaseGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, u); err != nil {
			t.Fatal(err)
		}
		if (u.GetDeletionTimestamp() != nil) != deleting {
			t.Errorf("%s: deleting = %v, want %v", name, u.GetDeletionTimestamp() != nil, deleting)
		}
	}
}
