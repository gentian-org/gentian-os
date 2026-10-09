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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// placed makes the harness's Component one the platform placed itself: it
// names no digest and carries the label ensureDefaultComponents gives it.
func (h *digestHarness) placed() *digestHarness {
	h.t.Helper()
	comp := &gentianov1alpha1.Component{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), comp); err != nil {
		h.t.Fatal(err)
	}
	comp.Labels = map[string]string{componentOriginLabel: componentOriginDefault}
	if err := h.c.Update(context.Background(), comp); err != nil {
		h.t.Fatal(err)
	}
	return h
}

// consoleProfile is a default profile as the installer places it: the
// document, its bytes beside it, and the catalogue they were fetched from.
func consoleProfile(t *testing.T) *gentianov1alpha1.ComponentProfile {
	t.Helper()
	p := materialised(t, wikiBundle)
	p.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
	return p
}

// A Component the platform placed names no digest. Its profile was placed at
// one, recorded as the bundle beside it, and is what that bundle says: it is
// rolled out.
func TestADefaultComponentWhoseProfileIsItsRecordedBuildRollsOut(t *testing.T) {
	h := startDigestHarness(t, consoleProfile(t), "").placed()
	got := h.reconcile()
	if ready := componentReadyCondition(got); ready == nil || ready.Reason != "Installing" {
		t.Fatalf("condition = %+v, want the release under way", ready)
	}
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("release at %q", h.releasedVersion())
	}
	if e := h.events(); len(e) != 0 {
		t.Fatalf("events for a default that verified: %v", e)
	}
}

// The profile is not what its recorded bundle says -- changed in git without
// the bundle, or in the cluster. Nothing is rendered from it, with the reason
// a pinned install is held with.
func TestADefaultComponentWhoseProfileIsNotItsRecordedBuildRendersNothing(t *testing.T) {
	profile := consoleProfile(t)
	profile.Spec.Package.Chart.Version = "6.6.6"
	profile.Spec.Package.Chart.Repository = "oci://elsewhere.invalid/wiki"
	h := startDigestHarness(t, profile, "").placed()

	ready := componentReadyCondition(h.reconcile())
	if ready == nil || ready.Status != "False" || ready.Reason != profilebundle.ReasonMismatch {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonMismatch)
	}
	for _, want := range []string{profilebundle.Short(profilebundle.Digest([]byte(wikiBundle))), "as recorded", "spec.package"} {
		if !strings.Contains(ready.Message, want) {
			t.Fatalf("the condition does not say %q: %s", want, ready.Message)
		}
	}
	if h.releasedVersion() != "" || h.networkPolicyWritten() {
		t.Fatalf("rendered from a profile that is not its recorded build: release %q, network policy %v",
			h.releasedVersion(), h.networkPolicyWritten())
	}
	if e := h.events(); len(e) != 1 || !strings.Contains(e[0], "Warning "+profilebundle.ReasonMismatch) {
		t.Fatalf("events = %v, want one warning", e)
	}
	h.reconcile()
	if e := h.events(); len(e) != 0 {
		t.Fatalf("the same mismatch was reported again: %v", e)
	}
}

// Changed after it was rolled out: the change is not followed, what is
// running stays, and the hold lifts when the profile is its build again.
func TestADefaultProfileChangedAfterRolloutIsCaughtAndNothingIsTakenDown(t *testing.T) {
	h := startDigestHarness(t, consoleProfile(t), "").placed()
	h.reconcile()
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("release at %q before the change", h.releasedVersion())
	}
	h.events()

	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) { p.Spec.Package.Chart.Version = "6.6.6" })
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != profilebundle.ReasonMismatch {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonMismatch)
	}
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("the release is now at %q: the changed profile was rolled out, or the release was removed", h.releasedVersion())
	}
	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) { p.Spec.Package.Chart.Version = "1.0.0" })
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason == profilebundle.ReasonMismatch {
		t.Fatalf("condition = %+v after the profile was put back", ready)
	}
}

// A companion the recorded bundle brings holds a default as it holds a
// pinned install, and is looked for again.
func TestADefaultComponentWaitsForItsBundlesCompanions(t *testing.T) {
	bundle := wikiBundle + "---\n" + wikiPage
	profile := materialised(t, bundle)
	profile.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
	h := startDigestHarness(t, profile, "").placed()

	result, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.comp)})
	if err != nil {
		t.Fatal(err)
	}
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != profilebundle.ReasonCompanionMissing {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonCompanionMissing)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("a default waiting for a companion does not look again")
	}
	if err := h.c.Create(context.Background(), wikiPageObject()); err != nil {
		t.Fatal(err)
	}
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != "Installing" {
		t.Fatalf("condition = %+v once the companion is there", ready)
	}
}

// A profile that says it came from a catalogue and shows no build is held:
// taking the bundle away is not the way round the check.
func TestADefaultComponentWhoseProfileLostItsBundleIsHeld(t *testing.T) {
	profile := consoleProfile(t)
	delete(profile.Annotations, profilebundle.Annotation)
	h := startDigestHarness(t, profile, "").placed()
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != profilebundle.ReasonUnverifiable {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonUnverifiable)
	}
	if h.releasedVersion() != "" {
		t.Fatal("rendered from a profile nothing vouches for")
	}
}

// Two that are not asked, as before: a default whose profile the platform's
// chart ships, which has no record of any kind, and an install that named no
// digest, whatever its profile carries.
func TestWhatHasNoRecordAndWhatIsNoDefaultAreNotAsked(t *testing.T) {
	shipped := materialised(t, wikiBundle)
	shipped.Annotations = nil
	shipped.Spec.Package.Chart.Version = "3.0.0"
	h := startDigestHarness(t, shipped, "").placed()
	h.reconcile()
	if h.releasedVersion() != "3.0.0" {
		t.Fatalf("a default with no record was not rolled out: release %q", h.releasedVersion())
	}

	changed := consoleProfile(t)
	changed.Spec.Package.Chart.Version = "3.0.0"
	h = startDigestHarness(t, changed, "")
	h.reconcile()
	if h.releasedVersion() != "3.0.0" {
		t.Fatalf("an install with no digest was held: release %q", h.releasedVersion())
	}
}
