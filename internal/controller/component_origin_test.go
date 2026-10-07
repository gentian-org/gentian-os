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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// from is a materialised profile recorded as coming from a catalogue.
func from(t *testing.T, bundle, origin string) *gentianov1alpha1.ComponentProfile {
	t.Helper()
	p := materialised(t, bundle)
	p.Annotations[profilebundle.OriginAnnotation] = origin
	return p
}

// A profile says which catalogue it came from, and a tenant's own catalogue
// belongs to that tenant. The origin the director writes beside the bundle
// does not disturb the check of the bundle itself: a pinned install from a
// catalogue of the whole cluster, or from the tenant's own, rolls out.
func TestAProfileOfTheClustersOrTheTenantsOwnCatalogueRollsOut(t *testing.T) {
	for _, origin := range []string{"cluster/gentian", "tenant/" + acmeTenantFixture().Name + "/ours"} {
		h := startDigestHarness(t, from(t, wikiBundle, origin), profilebundle.Digest([]byte(wikiBundle)))
		got := h.reconcile()
		if ready := componentReadyCondition(got); ready == nil || ready.Reason != "Installing" {
			t.Fatalf("%s: condition = %+v, want the release under way", origin, ready)
		}
		if h.releasedVersion() != "1.0.0" {
			t.Fatalf("%s: release at %q", origin, h.releasedVersion())
		}
	}
}

// A profile from another tenant's own catalogue is not rolled out, pinned or
// not: the object is visible to the whole cluster, so the director's refusal
// is not the only one. Nothing is rendered, and the condition says why.
func TestAProfileOfAnotherTenantsCatalogueRendersNothing(t *testing.T) {
	for name, pinned := range map[string]string{"pinned": profilebundle.Digest([]byte(wikiBundle)), "by name": ""} {
		h := startDigestHarness(t, from(t, wikiBundle, "tenant/somebody-else/ours"), pinned)
		got := h.reconcile()
		ready := componentReadyCondition(got)
		if ready == nil || ready.Status != "False" || ready.Reason != profilebundle.ReasonOtherTenant {
			t.Fatalf("%s: condition = %+v, want %s", name, ready, profilebundle.ReasonOtherTenant)
		}
		if !strings.Contains(ready.Message, "another tenant's own catalogue") || !strings.Contains(ready.Message, "nothing is rolled out") {
			t.Fatalf("%s: the condition does not say why: %s", name, ready.Message)
		}
		if strings.Contains(ready.Message, "somebody-else") {
			t.Fatalf("%s: the condition names the other tenant: %s", name, ready.Message)
		}
		if h.releasedVersion() != "" || h.networkPolicyWritten() {
			t.Fatalf("%s: rendered from another tenant's profile: release %q, network policy %v",
				name, h.releasedVersion(), h.networkPolicyWritten())
		}
		if e := h.events(); len(e) != 1 || !strings.Contains(e[0], "Warning "+profilebundle.ReasonOtherTenant) {
			t.Fatalf("%s: events = %v, want one warning", name, e)
		}
		// Said once.
		h.reconcile()
		if e := h.events(); len(e) != 0 {
			t.Fatalf("%s: the refusal was repeated: %v", name, e)
		}
	}
}

// An origin nobody can read is not an origin that admits everybody.
func TestAProfileWhoseOriginCannotBeReadRendersNothing(t *testing.T) {
	h := startDigestHarness(t, from(t, wikiBundle, "tenant/acme"), "")
	got := h.reconcile()
	if ready := componentReadyCondition(got); ready == nil || ready.Reason != profilebundle.ReasonOtherTenant || !strings.Contains(ready.Message, "cannot be read") {
		t.Fatalf("condition = %+v", ready)
	}
	if h.releasedVersion() != "" {
		t.Fatalf("rendered from a profile whose origin cannot be read: %q", h.releasedVersion())
	}
}

// An add-on switched on by name whose profile is not on the cluster holds
// the app it is switched on in, and says what to do: profiles are not on a
// cluster ahead of an install, and an add-on that resolves to nothing must
// not be dropped from the release without a word.
func TestAnAddonWithNoProfileOnTheClusterHoldsItsApp(t *testing.T) {
	h := startDigestHarness(t, materialised(t, wikiBundle), "")
	h.comp.Spec.Addons = []string{"wiki-search"}
	if err := h.c.Update(context.Background(), h.comp); err != nil {
		t.Fatal(err)
	}
	got := h.reconcile()
	ready := componentReadyCondition(got)
	if ready == nil || ready.Status != "False" || ready.Reason != reasonAddonProfileMissing {
		t.Fatalf("condition = %+v, want %s", ready, reasonAddonProfileMissing)
	}
	for _, want := range []string{"add-on wiki-search", "not on this cluster", "coordinate and digest", "<catalogue>/wiki-search"} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("the condition does not say %q: %s", want, ready.Message)
		}
	}
	if h.releasedVersion() != "" {
		t.Fatalf("rolled out without the add-on: %q", h.releasedVersion())
	}

	// Its profile arrives, from another tenant's own catalogue: still held.
	addon := &gentianov1alpha1.ComponentProfile{}
	addon.Name = "wiki-search"
	addon.Annotations = map[string]string{profilebundle.OriginAnnotation: "tenant/somebody-else/ours"}
	if err := h.c.Create(context.Background(), addon); err != nil {
		t.Fatal(err)
	}
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != profilebundle.ReasonOtherTenant || !strings.Contains(ready.Message, "add-on wiki-search") {
		t.Fatalf("with another tenant's add-on: condition = %+v", ready)
	}
	// From a catalogue of the cluster: rolled out.
	addon.Annotations[profilebundle.OriginAnnotation] = "cluster/gentian"
	if err := h.c.Update(context.Background(), addon); err != nil {
		t.Fatal(err)
	}
	if ready := componentReadyCondition(h.reconcile()); ready == nil || ready.Reason != "Installing" {
		t.Fatalf("with the add-on's profile on the cluster: condition = %+v", ready)
	}
}
