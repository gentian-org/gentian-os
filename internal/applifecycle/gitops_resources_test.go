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

package applifecycle

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func testPlan(quotas gentianov1alpha1.TenantQuotas) *gentianov1alpha1.ResourcePlan {
	return &gentianov1alpha1.ResourcePlan{
		ObjectMeta: metav1.ObjectMeta{Name: "base-plus-8"},
		Spec:       gentianov1alpha1.ResourcePlanSpec{DisplayName: "Base + 8", Quotas: quotas},
	}
}

func mustQty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// The patch must null the keys the plan leaves unset. Omitting them lets the
// tenant-defaults component's value survive the merge, and the tenant then runs
// on the plan's CPU with the default's storage while being billed for the plan.
func TestRenderPatchNullsQuotaKeysThePlanDoesNotSet(t *testing.T) {
	out := renderResourcePlanPatch("corp", testPlan(gentianov1alpha1.TenantQuotas{
		CPU:    mustQty("40"),
		Memory: mustQty("48Gi"),
	}))

	for _, want := range []string{
		`cpu: "40"`,
		`memory: "48Gi"`,
		"requestsCpu: null",
		"requestsMemory: null",
		"storage: null",
		"maxPods: null",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the patch to contain %q:\n%s", want, out)
		}
	}
}

// A bare 40 is an integer to YAML and the Tenant CRD wants a quantity string.
func TestRenderPatchQuotesQuantities(t *testing.T) {
	out := renderResourcePlanPatch("corp", testPlan(gentianov1alpha1.TenantQuotas{
		CPU: mustQty("40"),
	}))
	if strings.Contains(out, "cpu: 40\n") {
		t.Errorf("quantities must be quoted:\n%s", out)
	}
}

func TestRenderPatchCarriesThePlanAnnotation(t *testing.T) {
	out := renderResourcePlanPatch("corp", testPlan(gentianov1alpha1.TenantQuotas{CPU: mustQty("40")}))
	want := gentianov1alpha1.ResourcePlanAnnotation + ": base-plus-8"
	if !strings.Contains(out, want) {
		t.Errorf("expected %q in:\n%s", want, out)
	}
}

// Counts are integers in the CRD, so unlike quantities they must not be quoted.
func TestRenderPatchLeavesCountsUnquoted(t *testing.T) {
	out := renderResourcePlanPatch("corp", testPlan(gentianov1alpha1.TenantQuotas{
		MaxPods: 150,
	}))
	if !strings.Contains(out, "maxPods: 150\n") {
		t.Errorf("counts must be plain integers:\n%s", out)
	}
}

// A plan must not touch the app cap in either direction. Writing it would sell
// a policy limit as capacity; nulling it would have a plan change quietly
// delete the cluster's cap.
func TestRenderPatchNeverWritesTheAppCap(t *testing.T) {
	out := renderResourcePlanPatch("corp", testPlan(gentianov1alpha1.TenantQuotas{
		RequestsCPU: mustQty("2"),
		MaxApps:     30,
	}))
	if strings.Contains(out, "maxApps") {
		t.Errorf("the patch must not mention maxApps at all:\n%s", out)
	}
}

func TestEnsurePatchListedAppendsToAnExistingList(t *testing.T) {
	in := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- tenant.yaml
components:
- ../../definitions/components/tenant-defaults
patches:
- path: something-else.yaml
`
	out, changed := ensurePatchListed(in, resourcePlanPatchFile)
	if !changed {
		t.Fatal("expected the kustomization to change")
	}
	// Order matters: the plan must be the last patch applied, or an earlier one
	// touching quotas would win over the chosen plan.
	first := strings.Index(out, "something-else.yaml")
	second := strings.Index(out, resourcePlanPatchFile)
	if first < 0 || second < 0 || second < first {
		t.Fatalf("expected the plan patch last:\n%s", out)
	}
}

func TestEnsurePatchListedCreatesTheListWhenAbsent(t *testing.T) {
	in := `resources:
- tenant.yaml
components:
- ../../definitions/components/tenant-defaults
`
	out, changed := ensurePatchListed(in, resourcePlanPatchFile)
	if !changed {
		t.Fatal("expected the kustomization to change")
	}
	if !strings.Contains(out, "patches:\n- path: "+resourcePlanPatchFile) {
		t.Fatalf("expected a patches list to be created:\n%s", out)
	}
	// The components entry must survive: it is what applies the tenant defaults
	// the plan patch then narrows.
	if !strings.Contains(out, "tenant-defaults") {
		t.Fatalf("expected components to be preserved:\n%s", out)
	}
}

func TestEnsurePatchListedIsIdempotent(t *testing.T) {
	in := "resources:\n- tenant.yaml\npatches:\n- path: " + resourcePlanPatchFile + "\n"
	out, changed := ensurePatchListed(in, resourcePlanPatchFile)
	if changed || out != in {
		t.Fatalf("a second call must not change the file:\n%s", out)
	}
}

// The comments in these files explain why each component is pulled in. A
// load/dump round trip would drop them and make every plan change an
// unreviewable full-file diff.
func TestEnsurePatchListedPreservesComments(t *testing.T) {
	in := `resources:
- tenant.yaml
# The shared defaults every tenant on this cluster starts from.
components:
- ../../definitions/components/tenant-defaults
`
	out, _ := ensurePatchListed(in, resourcePlanPatchFile)
	if !strings.Contains(out, "# The shared defaults every tenant") {
		t.Fatalf("expected comments to survive:\n%s", out)
	}
}

// A plan sells reserved capacity and caps burst separately, so the patch has to
// carry both. Emitting only the limits pair would leave the tenant-defaults
// requests value in force — the tenant would reserve whatever the cluster
// default says while being billed for the plan's node count.
func TestRenderPatchCarriesRequestsAndLimits(t *testing.T) {
	out := renderResourcePlanPatch("corp", testPlan(gentianov1alpha1.TenantQuotas{
		RequestsCPU:    mustQty("2"),
		RequestsMemory: mustQty("4Gi"),
		CPU:            mustQty("8"),
		Memory:         mustQty("8Gi"),
	}))
	for _, want := range []string{
		`requestsCpu: "2"`,
		`requestsMemory: "4Gi"`,
		`cpu: "8"`,
		`memory: "8Gi"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the patch to contain %q:\n%s", want, out)
		}
	}
}
