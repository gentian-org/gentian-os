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
	"time"

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
