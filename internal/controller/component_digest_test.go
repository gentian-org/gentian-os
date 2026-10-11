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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// wikiBundle is a profile as a catalogue source publishes it: the bytes an
// install's digest is taken over.
const wikiBundle = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: wiki
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    chart:
      repository: oci://example.invalid/wiki
      name: wiki
      version: "1.0.0"
`

// materialised is the profile the cluster holds after the director committed
// a bundle and Argo CD applied it: the document, and its bytes in the
// annotation. This profile states nothing the schema would default.
func materialised(t *testing.T, bundle string) *gentianov1alpha1.ComponentProfile {
	t.Helper()
	p := &gentianov1alpha1.ComponentProfile{}
	if err := yaml.Unmarshal([]byte(bundle), p); err != nil {
		t.Fatal(err)
	}
	p.Annotations = map[string]string{profilebundle.Annotation: profilebundle.Encode([]byte(bundle))}
	return p
}

// digestHarness is one tenant, one profile and one Component of it.
type digestHarness struct {
	t        *testing.T
	c        client.Client
	r        *ComponentReconciler
	comp     *gentianov1alpha1.Component
	recorder *record.FakeRecorder
}

func startDigestHarness(t *testing.T, profile *gentianov1alpha1.ComponentProfile, pinned string) *digestHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	tenant := acmeTenantFixture()
	comp := componentFor(profile.Name, tenantNamespaceName(tenant))
	comp.Spec.ProfileRef.Digest = pinned
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(tenant.DeepCopy(), profile, comp).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	recorder := record.NewFakeRecorder(16)
	return &digestHarness{t: t, c: c, comp: comp, recorder: recorder,
		r: &ComponentReconciler{Client: c, Scheme: scheme, KernelDomain: "k.example", Recorder: recorder}}
}

func (h *digestHarness) reconcile() *gentianov1alpha1.Component {
	h.t.Helper()
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(h.comp)}); err != nil {
		h.t.Fatal(err)
	}
	got := &gentianov1alpha1.Component{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), got); err != nil {
		h.t.Fatal(err)
	}
	return got
}

// releasedVersion is the chart version of the component's Release, or "" when
// there is no Release.
func (h *digestHarness) releasedVersion() string {
	h.t.Helper()
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	err := h.c.Get(context.Background(), types.NamespacedName{Name: releaseName(h.comp)}, release)
	if errors.IsNotFound(err) {
		return ""
	}
	if err != nil {
		h.t.Fatal(err)
	}
	version, _, _ := unstructured.NestedString(release.Object, "spec", "forProvider", "chart", "version")
	return version
}

func (h *digestHarness) networkPolicyWritten() bool {
	h.t.Helper()
	err := h.c.Get(context.Background(), types.NamespacedName{
		Name: componentNetworkPolicyName(h.comp), Namespace: h.comp.Namespace}, &networkingv1.NetworkPolicy{})
	if err != nil && !errors.IsNotFound(err) {
		h.t.Fatal(err)
	}
	return err == nil
}

func (h *digestHarness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func (h *digestHarness) editProfile(edit func(*gentianov1alpha1.ComponentProfile)) {
	h.t.Helper()
	p := &gentianov1alpha1.ComponentProfile{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: h.comp.Spec.ProfileRef.Name}, p); err != nil {
		h.t.Fatal(err)
	}
	edit(p)
	if err := h.c.Update(context.Background(), p); err != nil {
		h.t.Fatal(err)
	}
}

// The profile the cluster holds is the build the install is pinned to: it is
// rolled out.
func TestAComponentPinnedToTheProfilesDigestRollsOut(t *testing.T) {
	h := startDigestHarness(t, materialised(t, wikiBundle), profilebundle.Digest([]byte(wikiBundle)))
	got := h.reconcile()
	if ready := componentReadyCondition(got); ready == nil || ready.Reason != "Installing" {
		t.Fatalf("condition = %+v, want the release under way", ready)
	}
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("release at %q", h.releasedVersion())
	}
	if e := h.events(); len(e) != 0 {
		t.Fatalf("events for an install that verified: %v", e)
	}
}

// The install is pinned to one build and the cluster holds another, bundle
// and all -- which is what another tenant installing the same entry at a
// newer digest leaves behind. Nothing is rendered from it.
func TestAComponentPinnedToAnotherBuildRendersNothing(t *testing.T) {
	newer := strings.ReplaceAll(wikiBundle, `version: "1.0.0"`, `version: "2.0.0"`)
	pinned := profilebundle.Digest([]byte(wikiBundle))
	h := startDigestHarness(t, materialised(t, newer), pinned)

	got := h.reconcile()
	ready := componentReadyCondition(got)
	if ready == nil || ready.Status != "False" || ready.Reason != profilebundle.ReasonMismatch {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonMismatch)
	}
	// Expected and found, short, so the two builds can be told apart.
	for _, want := range []string{profilebundle.Short(pinned), profilebundle.Short(profilebundle.Digest([]byte(newer)))} {
		if !strings.Contains(ready.Message, want) {
			t.Fatalf("the condition does not name %s: %s", want, ready.Message)
		}
	}
	if h.releasedVersion() != "" || h.networkPolicyWritten() {
		t.Fatalf("rendered from a profile that is not the pinned build: release %q, network policy %v",
			h.releasedVersion(), h.networkPolicyWritten())
	}
	e := h.events()
	if len(e) != 1 || !strings.Contains(e[0], "Warning "+profilebundle.ReasonMismatch) {
		t.Fatalf("events = %v, want one warning", e)
	}
	// Said once: the next pass finds the same thing and does not repeat it.
	h.reconcile()
	if e := h.events(); len(e) != 0 {
		t.Fatalf("the same mismatch was reported again: %v", e)
	}
}

// The profile was the pinned build when it was rolled out, and was changed
// afterwards -- in the cluster, with its bundle left as it was. The bytes
// still hash to the pin; the profile is no longer what they say. The change
// is not rolled out, and what is running is not taken down.
func TestAProfileChangedAfterInstallIsCaughtAndNothingIsTakenDown(t *testing.T) {
	pinned := profilebundle.Digest([]byte(wikiBundle))
	h := startDigestHarness(t, materialised(t, wikiBundle), pinned)
	h.reconcile()
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("release at %q before the change", h.releasedVersion())
	}
	h.events()

	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) {
		p.Spec.Package.Chart.Version = "6.6.6"
		p.Spec.Package.Chart.Repository = "oci://elsewhere.invalid/wiki"
	})
	got := h.reconcile()
	ready := componentReadyCondition(got)
	if ready == nil || ready.Reason != profilebundle.ReasonMismatch || !strings.Contains(ready.Message, "spec.package") {
		t.Fatalf("condition = %+v, want %s naming spec.package", ready, profilebundle.ReasonMismatch)
	}
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("the release is now at %q: the changed profile was rolled out, or the release was removed", h.releasedVersion())
	}
	if e := h.events(); len(e) != 1 {
		t.Fatalf("events = %v, want one", e)
	}

	// Put back, it is the pinned build again and the hold lifts by itself.
	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) {
		p.Spec.Package.Chart.Version = "1.0.0"
		p.Spec.Package.Chart.Repository = "oci://example.invalid/wiki"
	})
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason == profilebundle.ReasonMismatch {
		t.Fatalf("condition = %+v after the profile was put back", ready)
	}
}

// Taking the bundle away does not take the check away: a pinned install whose
// profile has nothing to be checked against is held, not waved through.
func TestAPinnedComponentWhoseProfileCarriesNoBundleIsHeld(t *testing.T) {
	profile := materialised(t, wikiBundle)
	profile.Annotations = nil
	h := startDigestHarness(t, profile, profilebundle.Digest([]byte(wikiBundle)))
	ready := componentReadyCondition(h.reconcile())
	if ready == nil || ready.Reason != profilebundle.ReasonUnverifiable {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonUnverifiable)
	}
	if h.releasedVersion() != "" || h.networkPolicyWritten() {
		t.Fatal("rendered from a profile nothing vouches for")
	}
}

// An install that names no digest is not pinned, and is rolled out as it
// always was: whatever profile the cluster holds under that name, with or
// without a bundle, changed or not.
func TestAComponentWithNoDigestIsRolledOutAsBefore(t *testing.T) {
	plain := materialised(t, wikiBundle)
	plain.Annotations = nil
	h := startDigestHarness(t, plain, "")
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != "Installing" {
		t.Fatalf("condition = %+v", ready)
	}
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("release at %q", h.releasedVersion())
	}

	h.editProfile(func(p *gentianov1alpha1.ComponentProfile) { p.Spec.Package.Chart.Version = "2.0.0" })
	h.reconcile()
	if h.releasedVersion() != "2.0.0" {
		t.Fatalf("release at %q: an unpinned install follows its profile", h.releasedVersion())
	}
	if e := h.events(); len(e) != 0 {
		t.Fatalf("events = %v", e)
	}
}

// talkBundle is an addon as a catalogue source publishes it: it deploys
// nothing and activates inside wiki.
const talkBundle = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: wiki-talk
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    addon:
      of: wiki
`

// startAddonHarness is the digest harness with wiki as an unpinned base that
// activates wiki-talk, pinned or not. addon is the addon's profile as the
// cluster holds it, nil for a cluster that does not hold it.
func startAddonHarness(t *testing.T, addon *gentianov1alpha1.ComponentProfile, pinned string) *digestHarness {
	t.Helper()
	h := startDigestHarness(t, materialised(t, wikiBundle), "")
	if addon != nil {
		if err := h.c.Create(context.Background(), addon); err != nil {
			t.Fatal(err)
		}
	}
	base := &gentianov1alpha1.Component{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), base); err != nil {
		t.Fatal(err)
	}
	base.Spec.Addons = []string{"wiki-talk"}
	if pinned != "" {
		base.Spec.AddonPins = []gentianov1alpha1.AddonPin{{Name: "wiki-talk", Digest: pinned, Catalogue: "main"}}
	}
	if err := h.c.Update(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	return h
}

// An addon takes effect in its base's release, so its pin is checked where
// the base is rolled out. The addon's profile is the pinned build: the base
// is rolled out.
func TestABaseWithAVerifiedPinnedAddonRollsOut(t *testing.T) {
	h := startAddonHarness(t, materialised(t, talkBundle), profilebundle.Digest([]byte(talkBundle)))
	got := h.reconcile()
	if ready := componentReadyCondition(got); ready == nil || ready.Reason != "Installing" {
		t.Fatalf("condition = %+v, want the release under way", ready)
	}
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("release at %q", h.releasedVersion())
	}
	if e := h.events(); len(e) != 0 {
		t.Fatalf("events for an addon that verified: %v", e)
	}
}

// The addon is pinned to one build and the cluster holds another. Nothing is
// rendered for the base, so the addon reaches no release values; the
// condition names the addon and both builds.
func TestABaseIsHeldWhileAPinnedAddonIsAnotherBuild(t *testing.T) {
	newer := strings.ReplaceAll(talkBundle, `version: "1.0.0"`, `version: "2.0.0"`)
	pinned := profilebundle.Digest([]byte(talkBundle))
	h := startAddonHarness(t, materialised(t, newer), pinned)

	got := h.reconcile()
	ready := componentReadyCondition(got)
	if ready == nil || ready.Status != "False" || ready.Reason != profilebundle.ReasonMismatch {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonMismatch)
	}
	for _, want := range []string{"addon wiki-talk", profilebundle.Short(pinned), profilebundle.Short(profilebundle.Digest([]byte(newer)))} {
		if !strings.Contains(ready.Message, want) {
			t.Fatalf("the condition does not name %s: %s", want, ready.Message)
		}
	}
	if h.releasedVersion() != "" || h.networkPolicyWritten() {
		t.Fatalf("rendered for a base whose addon is not the pinned build: release %q, network policy %v",
			h.releasedVersion(), h.networkPolicyWritten())
	}
	if e := h.events(); len(e) != 1 || !strings.Contains(e[0], "Warning "+profilebundle.ReasonMismatch) {
		t.Fatalf("events = %v, want one warning", e)
	}

	// The addon's profile becomes the pinned build again: the base is
	// released.
	current := &gentianov1alpha1.ComponentProfile{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "wiki-talk"}, current); err != nil {
		t.Fatal(err)
	}
	restored := materialised(t, talkBundle)
	restored.ResourceVersion = current.ResourceVersion
	if err := h.c.Update(context.Background(), restored); err != nil {
		t.Fatal(err)
	}
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != "Installing" {
		t.Fatalf("condition = %+v, want the release under way once the addon verifies", ready)
	}
}

// A base that was rolled out is not taken down when an addon's profile stops
// being the pinned build: it is held as it runs.
func TestAnAddonChangedAfterInstallHoldsTheBaseAsItRuns(t *testing.T) {
	h := startAddonHarness(t, materialised(t, talkBundle), profilebundle.Digest([]byte(talkBundle)))
	h.reconcile()
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("release at %q", h.releasedVersion())
	}
	current := &gentianov1alpha1.ComponentProfile{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Name: "wiki-talk"}, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Version = "9.9.9"
	if err := h.c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != profilebundle.ReasonMismatch {
		t.Fatalf("condition = %+v, want %s", ready, profilebundle.ReasonMismatch)
	}
	if h.releasedVersion() != "1.0.0" {
		t.Fatalf("the release was taken down or changed: %q", h.releasedVersion())
	}
}

// A pinned addon with nothing to check it against holds the base too: a
// profile with no bundle, and a profile the cluster does not hold.
func TestABaseIsHeldWhileAPinnedAddonCannotBeVerified(t *testing.T) {
	pinned := profilebundle.Digest([]byte(talkBundle))
	bare := materialised(t, talkBundle)
	bare.Annotations = nil
	for name, addon := range map[string]*gentianov1alpha1.ComponentProfile{"no bundle": bare, "no profile": nil} {
		h := startAddonHarness(t, addon, pinned)
		ready := componentReadyCondition(h.reconcile())
		if ready == nil || ready.Status != "False" || ready.Reason != profilebundle.ReasonUnverifiable ||
			!strings.Contains(ready.Message, "addon wiki-talk") {
			t.Fatalf("%s: condition = %+v, want %s naming the addon", name, ready, profilebundle.ReasonUnverifiable)
		}
		if h.releasedVersion() != "" {
			t.Fatalf("%s: rendered for a base whose addon could not be verified", name)
		}
	}
}

// An addon with no pin is not checked, like an app installed with no digest:
// whatever its profile is, the base is rolled out as before. And a pin for an
// addon the base does not activate holds nothing.
func TestAnUnpinnedAddonIsActivatedAsBefore(t *testing.T) {
	bare := materialised(t, talkBundle)
	bare.Annotations = nil
	h := startAddonHarness(t, bare, "")
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != "Installing" {
		t.Fatalf("condition = %+v, want the release under way", ready)
	}

	// (With its profile on the cluster: an add-on that has none holds the
	// base whatever is pinned, component_origin_test.go.)
	again := materialised(t, talkBundle)
	again.Annotations = nil
	h = startAddonHarness(t, again, "")
	base := &gentianov1alpha1.Component{}
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(h.comp), base); err != nil {
		t.Fatal(err)
	}
	base.Spec.AddonPins = []gentianov1alpha1.AddonPin{{Name: "something-else", Digest: profilebundle.Digest([]byte(talkBundle))}}
	if err := h.c.Update(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != "Installing" {
		t.Fatalf("a pin for an addon that is not activated held the base: %+v", ready)
	}
}
