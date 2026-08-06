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

package customization

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func fullJustification(upTo int) map[string]string {
	rungs := []string{"L0", "L1", "L2", "L3", "L4", "L5"}
	out := map[string]string{}
	for i := 0; i < upTo && i < len(rungs); i++ {
		out[rungs[i]] = "considered and rejected"
	}
	return out
}

func TestSupportsRungCompanionAlwaysAvailable(t *testing.T) {
	// L2 is a property of the customization, not of the target: a companion app is
	// always buildable, which is why it never appears in supportedRungs.
	surfaces := []*gentianov1alpha1.CustomizationSurface{
		nil,
		{SupportedRungs: []gentianov1alpha1.CustomizationRung{gentianov1alpha1.RungConfigure}},
		{Grade: gentianov1alpha1.GradeD},
	}
	for _, surface := range surfaces {
		if !SupportsRung(surface, gentianov1alpha1.RungCompanion) {
			t.Fatalf("L2 must always be available, surface=%+v", surface)
		}
	}
}

func TestSupportsRungDefaultsWhenUndeclared(t *testing.T) {
	// An uncharacterised app may only be configured or repackaged.
	if !SupportsRung(nil, gentianov1alpha1.RungConfigure) {
		t.Fatal("L0 must be available by default")
	}
	if !SupportsRung(nil, gentianov1alpha1.RungRepackage) {
		t.Fatal("L4 must be available by default")
	}
	if SupportsRung(nil, gentianov1alpha1.RungExtension) {
		t.Fatal("L3 must not be inferred for an uncharacterised app")
	}
	if SupportsRung(nil, gentianov1alpha1.RungPatch) {
		t.Fatal("L5 must not be inferred for an uncharacterised app")
	}
}

func TestLowestSupportedRungPicksCheapest(t *testing.T) {
	surface := &gentianov1alpha1.CustomizationSurface{
		SupportedRungs: []gentianov1alpha1.CustomizationRung{
			gentianov1alpha1.RungConfigure,
			gentianov1alpha1.RungDropIn,
			gentianov1alpha1.RungExtension,
		},
	}
	got, ok := LowestSupportedRung(surface, []gentianov1alpha1.CustomizationRung{
		gentianov1alpha1.RungExtension,
		gentianov1alpha1.RungDropIn,
	})
	if !ok || got != gentianov1alpha1.RungDropIn {
		t.Fatalf("expected L1, got %q (ok=%v)", got, ok)
	}
}

func TestValidateRecordRejectsPatchAtTenantScope(t *testing.T) {
	record := &gentianov1alpha1.Customization{
		ObjectMeta: metav1.ObjectMeta{Name: "bad"},
		Spec: gentianov1alpha1.CustomizationSpec{
			Summary:           "patch for one tenant",
			Target:            gentianov1alpha1.CustomizationTarget{Profile: "odoo-cb-base"},
			Rung:              gentianov1alpha1.RungPatch,
			Scope:             gentianov1alpha1.ScopeTenant,
			Tenants:           []string{"acme"},
			Owner:             "platform-erp",
			ReviewBy:          "2027-01-01",
			ExitCriteria:      "upstream fix",
			RungJustification: fullJustification(5),
			UpstreamFirst: &gentianov1alpha1.CustomizationUpstreamFirst{
				Attempted: true,
				Forwarded: gentianov1alpha1.CustomizationForwarded("yes"),
			},
			Artifacts: []gentianov1alpha1.CustomizationArtifact{{Repo: "gentian-org/ocb"}},
		},
	}
	errs := ValidateRecord(record)
	if !containsMessage(errs, "not permitted at spec.scope tenant") {
		t.Fatalf("expected tenant-scope rejection, got %v", errs)
	}
}

func TestValidateRecordRequiresUpstreamFirstFromL4(t *testing.T) {
	record := &gentianov1alpha1.Customization{
		Spec: gentianov1alpha1.CustomizationSpec{
			Summary:           "wrapper chart",
			Target:            gentianov1alpha1.CustomizationTarget{Profile: "xwiki"},
			Rung:              gentianov1alpha1.RungRepackage,
			Scope:             gentianov1alpha1.ScopeProfile,
			Owner:             "platform",
			ReviewBy:          "2027-01-01",
			RungJustification: fullJustification(4),
			Artifacts:         []gentianov1alpha1.CustomizationArtifact{{Repo: "gentian-org/gentian-apps"}},
		},
	}
	errs := ValidateRecord(record)
	if !containsMessage(errs, "upstreamFirst.attempted must be true") {
		t.Fatalf("expected upstream-first requirement, got %v", errs)
	}
}

func TestValidateRecordRequiresReasonForForwardedNo(t *testing.T) {
	record := &gentianov1alpha1.Customization{
		Spec: gentianov1alpha1.CustomizationSpec{
			Summary:           "wrapper chart",
			Target:            gentianov1alpha1.CustomizationTarget{Profile: "xwiki"},
			Rung:              gentianov1alpha1.RungRepackage,
			Scope:             gentianov1alpha1.ScopeProfile,
			Owner:             "platform",
			ReviewBy:          "2027-01-01",
			RungJustification: fullJustification(4),
			Artifacts:         []gentianov1alpha1.CustomizationArtifact{{Repo: "gentian-org/gentian-apps"}},
			UpstreamFirst: &gentianov1alpha1.CustomizationUpstreamFirst{
				Attempted: true,
				Forwarded: gentianov1alpha1.CustomizationForwarded("no"),
			},
		},
	}
	errs := ValidateRecord(record)
	if !containsMessage(errs, "reason is required") {
		t.Fatalf("expected reason requirement, got %v", errs)
	}
}

func TestValidateRecordRequiresJustificationChain(t *testing.T) {
	record := &gentianov1alpha1.Customization{
		Spec: gentianov1alpha1.CustomizationSpec{
			Summary:   "module",
			Target:    gentianov1alpha1.CustomizationTarget{Profile: "odoo-cb-base"},
			Rung:      gentianov1alpha1.RungExtension,
			Scope:     gentianov1alpha1.ScopeProfile,
			Owner:     "platform-erp",
			ReviewBy:  "2027-01-01",
			Artifacts: []gentianov1alpha1.CustomizationArtifact{{Repo: "gentian-org/odoo-modules"}},
			// L0/L1/L2 justifications deliberately missing.
		},
	}
	errs := ValidateRecord(record)
	for _, rung := range []string{"L0", "L1", "L2"} {
		if !containsMessage(errs, "rungJustification[\""+rung+"\"]") {
			t.Fatalf("expected missing justification for %s, got %v", rung, errs)
		}
	}
}

func TestValidateRecordAcceptsWellFormedL3(t *testing.T) {
	record := &gentianov1alpha1.Customization{
		Spec: gentianov1alpha1.CustomizationSpec{
			Summary:           "invoice approval",
			Target:            gentianov1alpha1.CustomizationTarget{Profile: "odoo-cb-base"},
			Rung:              gentianov1alpha1.RungExtension,
			Scope:             gentianov1alpha1.ScopeProfile,
			Owner:             "platform-erp",
			ReviewBy:          "2027-02-06",
			RungJustification: fullJustification(3),
			Artifacts:         []gentianov1alpha1.CustomizationArtifact{{Repo: "gentian-org/odoo-modules"}},
		},
	}
	if errs := ValidateRecord(record); len(errs) != 0 {
		t.Fatalf("expected no violations, got %v", errs)
	}
}

func TestValidateRecordRejectsTenantScopeWithoutTenants(t *testing.T) {
	record := &gentianov1alpha1.Customization{
		Spec: gentianov1alpha1.CustomizationSpec{
			Summary:           "branding",
			Target:            gentianov1alpha1.CustomizationTarget{Profile: "odoo-cb-base"},
			Rung:              gentianov1alpha1.RungDropIn,
			Scope:             gentianov1alpha1.ScopeTenant,
			Owner:             "acme-admin",
			ReviewBy:          "2027-01-01",
			RungJustification: fullJustification(1),
		},
	}
	if errs := ValidateRecord(record); !containsMessage(errs, "spec.tenants must be non-empty") {
		t.Fatalf("expected tenants requirement, got %v", errs)
	}
}

func TestValidateNamespaceScopingEnforcesTenantNamespace(t *testing.T) {
	record := &gentianov1alpha1.Customization{
		Spec: gentianov1alpha1.CustomizationSpec{
			Target:  gentianov1alpha1.CustomizationTarget{Profile: "odoo-cb-base"},
			Rung:    gentianov1alpha1.RungExtension,
			Scope:   gentianov1alpha1.ScopeTenant,
			Tenants: []string{"acme"},
		},
	}
	if err := ValidateNamespaceScoping(record, "tenant-acme", []string{"tenant-acme"}); err != nil {
		t.Fatalf("expected dedicated namespace to pass, got %v", err)
	}
	err := ValidateNamespaceScoping(record, "shared-erp", []string{"tenant-acme"})
	if err == nil || !strings.Contains(err.Error(), "shared runtime are") {
		t.Fatalf("expected shared-namespace rejection, got %v", err)
	}
}

func TestGradeForScoreBanding(t *testing.T) {
	cases := map[int32]gentianov1alpha1.CustomizationGrade{
		8: gentianov1alpha1.GradeA,
		7: gentianov1alpha1.GradeA,
		6: gentianov1alpha1.GradeB,
		5: gentianov1alpha1.GradeB,
		4: gentianov1alpha1.GradeC,
		3: gentianov1alpha1.GradeC,
		2: gentianov1alpha1.GradeD,
		0: gentianov1alpha1.GradeD,
	}
	for score, want := range cases {
		if got := GradeForScore(score); got != want {
			t.Fatalf("score %d: expected %q, got %q", score, want, got)
		}
	}
}

func TestValidateDropInContentRejectsMalformed(t *testing.T) {
	if err := ValidateDropInContent("yaml", "key: [unterminated"); err == nil {
		t.Fatal("expected malformed YAML to be rejected")
	}
	if err := ValidateDropInContent("json", `{"a":}`); err == nil {
		t.Fatal("expected malformed JSON to be rejected")
	}
	if err := ValidateDropInContent("ini", "[section\nkey = value"); err == nil {
		t.Fatal("expected unterminated section header to be rejected")
	}
	if err := ValidateDropInContent("ini", "# comment\n[section]\nkey = value"); err != nil {
		t.Fatalf("expected valid ini to pass, got %v", err)
	}
	if err := ValidateDropInContent("files", "\x00binary"); err != nil {
		t.Fatalf("opaque files must not be parsed, got %v", err)
	}
}

func containsMessage(errs []error, substr string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), substr) {
			return true
		}
	}
	return false
}
