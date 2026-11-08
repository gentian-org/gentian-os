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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TestParseLadderDateAcceptsYAMLNormalizedForm guards against a real failure
// seen live: a bare "2027-08-06" in a Customization record's YAML is parsed by
// sigs.k8s.io/yaml as a timestamp and normalized to "2027-08-06T00:00:00Z"
// before it reaches the API server, which then fails a strict
// ^\d{4}-\d{2}-\d{2}$ pattern. The CRD pattern was widened to accept both
// forms; this locks the Go-side parser in step with it.
func TestParseLadderDateAcceptsYAMLNormalizedForm(t *testing.T) {
	want := time.Date(2027, 8, 6, 0, 0, 0, 0, time.UTC)

	bare, err := parseLadderDate("2027-08-06")
	if err != nil || !bare.Equal(want) {
		t.Fatalf("bare date: got %v, %v; want %v", bare, err, want)
	}

	normalized, err := parseLadderDate("2027-08-06T00:00:00Z")
	if err != nil || !normalized.Equal(want) {
		t.Fatalf("YAML-normalized date: got %v, %v; want %v", normalized, err, want)
	}
}

func TestParseLadderDateRejectsGarbage(t *testing.T) {
	if _, err := parseLadderDate("not-a-date"); err == nil {
		t.Fatal("expected an error for an unparseable date")
	}
}

// TestRungAboveRecommendedIgnoresJustifiedRungs guards against a real failure
// seen live: a valid L3 record against a grade-A app (which supports L0/L1 for
// other purposes) was permanently flagged as "could descend" even though its
// spec.rungJustification already explained why L0/L1/L2 do not apply — an
// admission-time requirement for every record that passes validation. The
// signal must only fire for a cheaper rung the record's justification map has
// no entry for.
func TestRungAboveRecommendedIgnoresJustifiedRungs(t *testing.T) {
	surface := &gentianov1alpha1.CustomizationSurface{
		SupportedRungs: []gentianov1alpha1.CustomizationRung{
			gentianov1alpha1.RungConfigure,
			gentianov1alpha1.RungDropIn,
			gentianov1alpha1.RungExtension,
		},
	}
	record := &gentianov1alpha1.Customization{
		Spec: gentianov1alpha1.CustomizationSpec{
			Rung: gentianov1alpha1.RungExtension,
			RungJustification: map[string]string{
				"L0": "no configuration flag exists",
				"L1": "no drop-in mechanism applies",
				"L2": "must alter the app's own UI",
			},
		},
	}
	if rungAboveRecommended(record, surface) {
		t.Fatal("a record that justified every cheaper rung must not be flagged")
	}
}

func TestRungAboveRecommendedFlagsUnjustifiedNewCapability(t *testing.T) {
	surface := &gentianov1alpha1.CustomizationSurface{
		SupportedRungs: []gentianov1alpha1.CustomizationRung{
			gentianov1alpha1.RungConfigure,
			gentianov1alpha1.RungDropIn,
			gentianov1alpha1.RungExtension,
		},
	}
	record := &gentianov1alpha1.Customization{
		Spec: gentianov1alpha1.CustomizationSpec{
			Rung: gentianov1alpha1.RungExtension,
			RungJustification: map[string]string{
				"L0": "no configuration flag exists",
				// L1 deliberately missing: simulates a drop-in mechanism the
				// app gained after this record was written.
			},
		},
	}
	if !rungAboveRecommended(record, surface) {
		t.Fatal("expected a flag for a supported rung with no recorded justification")
	}
}

// TestCustomizationObservedGenerationFollowsAnUnchangedVerdict guards the
// short-circuit for an unchanged status: a spec edit that leaves the verdict
// as it was still has to advance observedGeneration, or the record reads as
// never having been looked at since the edit.
func TestCustomizationObservedGenerationFollowsAnUnchangedVerdict(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	record := &gentianov1alpha1.Customization{
		ObjectMeta: metav1.ObjectMeta{Name: "patch-a", Generation: 1},
		Spec: gentianov1alpha1.CustomizationSpec{
			Summary: "first",
			Target:  gentianov1alpha1.CustomizationTarget{Profile: "absent"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(record).
		WithStatusSubresource(record).Build()
	r := &CustomizationReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(record)}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	got := &gentianov1alpha1.Customization{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != 1 || got.Status.Phase != gentianov1alpha1.CustomizationPhaseInvalid {
		t.Fatalf("after the first pass: observedGeneration=%d phase=%q", got.Status.ObservedGeneration, got.Status.Phase)
	}

	// A spec edit that does not change the verdict: the target is still absent.
	got.Spec.Summary = "second"
	got.Generation = 2
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Generation != 2 {
		t.Fatalf("the test's own edit did not land: generation=%d", got.Generation)
	}
	if got.Status.ObservedGeneration != 2 {
		t.Fatalf("observedGeneration = %d after reconciling generation 2 with the same verdict", got.Status.ObservedGeneration)
	}
}
