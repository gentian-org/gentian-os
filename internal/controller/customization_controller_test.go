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
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/customization"
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

// An addon inherits its base's ladder. It carries only spec.customization.addon —
// restating grade or supportedRungs would fork a mutable fact — so reading its own
// surface yields the [L0, L4] default and rejects records the base plainly supports.
// CI resolved this inheritance and the operator did not, so the two disagreed about
// the same record.

func addonProfile(name, of string) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			Customization: &gentianov1alpha1.CustomizationSurface{
				Addon: &gentianov1alpha1.CustomizationAddon{ID: "crm", Of: of},
			},
		},
	}
}

func baseProfile(name string, rungs ...gentianov1alpha1.CustomizationRung) *gentianov1alpha1.AppProfile {
	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			Customization: &gentianov1alpha1.CustomizationSurface{
				Grade:          gentianov1alpha1.GradeA,
				SupportedRungs: rungs,
			},
		},
	}
}

func newLadderReconciler(objs ...client.Object) *CustomizationReconciler {
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	return &CustomizationReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Scheme: scheme,
	}
}

func TestLadderSurfaceFollowsAddonToItsBase(t *testing.T) {
	t.Parallel()
	base := baseProfile("odoo-base-ce",
		gentianov1alpha1.RungConfigure, gentianov1alpha1.RungExtension)
	addon := addonProfile("odoo-crm-ce", "odoo-base-ce")
	r := newLadderReconciler(base, addon)

	surface := r.ladderSurfaceFor(context.Background(), addon)
	if surface == nil || len(surface.SupportedRungs) != 2 {
		t.Fatalf("expected the base's rungs, got %+v", surface)
	}
	if !customization.SupportsRung(surface, gentianov1alpha1.RungExtension) {
		t.Fatal("addon must inherit L3 from its base — this is the bug being fixed")
	}
}

func TestLadderSurfaceLeavesANonAddonAlone(t *testing.T) {
	t.Parallel()
	// An edition shares a family name and nothing else, so it must be read as it
	// stands rather than borrowing another profile's reachability.
	od := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-base-od"},
		Spec: gentianov1alpha1.AppProfileSpec{
			Customization: &gentianov1alpha1.CustomizationSurface{
				Grade:          gentianov1alpha1.GradeUnknown,
				SupportedRungs: []gentianov1alpha1.CustomizationRung{gentianov1alpha1.RungConfigure, gentianov1alpha1.RungRepackage},
			},
		},
	}
	ce := baseProfile("nextcloud-base-ce",
		gentianov1alpha1.RungConfigure, gentianov1alpha1.RungExtension)
	r := newLadderReconciler(ce, od)

	surface := r.ladderSurfaceFor(context.Background(), od)
	if customization.SupportsRung(surface, gentianov1alpha1.RungExtension) {
		t.Fatal("an edition must not inherit L3 from a same-family profile")
	}
}

func TestLadderSurfaceFallsBackWhenTheBaseIsMissing(t *testing.T) {
	t.Parallel()
	addon := addonProfile("odoo-crm-ce", "odoo-base-ce")
	r := newLadderReconciler(addon) // base absent

	surface := r.ladderSurfaceFor(context.Background(), addon)
	if surface == nil {
		t.Fatal("expected the addon's own surface rather than nil")
	}
	if customization.SupportsRung(surface, gentianov1alpha1.RungExtension) {
		t.Fatal("a missing base must not grant rungs")
	}
}
