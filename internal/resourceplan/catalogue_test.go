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

package resourceplan

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func qty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func plan(name string, tier int32, cpu, memory string, opts ...func(*gentianov1alpha1.ResourcePlan)) gentianov1alpha1.ResourcePlan {
	p := gentianov1alpha1.ResourcePlan{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.ResourcePlanSpec{
			DisplayName: name,
			Tier:        tier,
			Quotas: gentianov1alpha1.TenantQuotas{
				CPU:    qty(cpu),
				Memory: qty(memory),
			},
		},
	}
	for _, opt := range opts {
		opt(&p)
	}
	return p
}

func testCatalogue() *Catalogue {
	return &Catalogue{Plans: []gentianov1alpha1.ResourcePlan{
		plan("base", 0, "32", "32Gi", func(p *gentianov1alpha1.ResourcePlan) {
			p.Spec.Default = true
			p.Spec.ProductSku = "sku-base"
		}),
		plan("base-plus-8", 20, "40", "48Gi"),
		plan("bespoke", 30, "64", "96Gi", func(p *gentianov1alpha1.ResourcePlan) {
			p.Spec.SelfServiceDisabled = true
		}),
	}}
}

func tenantWith(quotas *gentianov1alpha1.TenantQuotas, annotation string) *gentianov1alpha1.Tenant {
	t := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{Quotas: quotas},
	}
	if annotation != "" {
		t.Annotations = map[string]string{gentianov1alpha1.ResourcePlanAnnotation: annotation}
	}
	return t
}

// A ceiling written as 32Gi and one written as 32768Mi are the same ceiling.
// Matching on the rendered string would call the second one custom and quietly
// stop billing a tenant whose YAML someone reformatted.
func TestMatchComparesQuantitiesNotStrings(t *testing.T) {
	c := testCatalogue()
	got := c.Match(&gentianov1alpha1.TenantQuotas{CPU: qty("32"), Memory: qty("32768Mi")})
	if got == nil || got.Name != "base" {
		t.Fatalf("expected base, got %v", got)
	}
}

func TestResolveReportsDriftBetweenAnnotationAndQuotas(t *testing.T) {
	c := testCatalogue()
	tenant := tenantWith(&gentianov1alpha1.TenantQuotas{CPU: qty("32"), Memory: qty("32Gi")}, "base-plus-8")

	res := c.Resolve(tenant)
	if res.Plan == nil || res.Plan.Name != "base" {
		t.Fatalf("expected the enforced quotas to win, got %v", res.Plan)
	}
	if !res.Drifted {
		t.Fatal("expected drift to be reported when the annotation names another plan")
	}
	if res.Annotated != "base-plus-8" {
		t.Fatalf("expected the annotation to be surfaced, got %q", res.Annotated)
	}
}

// A tenant with no quotas at all is unbounded, not bespoke. Calling that custom
// would invite an operator to preserve a ceiling that does not exist.
func TestResolveTreatsAbsentQuotasAsTheDefaultPlan(t *testing.T) {
	res := testCatalogue().Resolve(tenantWith(nil, ""))
	if res.Custom {
		t.Fatal("absent quotas must not be reported as a custom ceiling")
	}
	if res.Plan == nil || res.Plan.Name != "base" {
		t.Fatalf("expected the default plan, got %v", res.Plan)
	}
}

func TestResolveReportsHandEditedCeilingAsCustom(t *testing.T) {
	res := testCatalogue().Resolve(
		tenantWith(&gentianov1alpha1.TenantQuotas{CPU: qty("37"), Memory: qty("41Gi")}, ""))
	if !res.Custom {
		t.Fatal("a ceiling matching no plan must be reported as custom")
	}
	if res.Plan != nil {
		t.Fatalf("expected no plan, got %v", res.Plan)
	}
}

func TestSelectableWithholdsBespokePlansFromSelfService(t *testing.T) {
	c := testCatalogue()
	names := func(plans []gentianov1alpha1.ResourcePlan) []string {
		out := make([]string, 0, len(plans))
		for _, p := range plans {
			out = append(out, p.Name)
		}
		return out
	}

	self := names(c.Selectable(true, nil))
	for _, n := range self {
		if n == "bespoke" {
			t.Fatal("a self-service caller must not be offered a bespoke plan")
		}
	}
	if len(names(c.Selectable(false, nil))) != 3 {
		t.Fatal("a cluster operator sees the whole catalogue")
	}
}

func TestSelectableAppliesTheEntitlementCeiling(t *testing.T) {
	var maxTier int32 = 20
	got := testCatalogue().Selectable(true, &maxTier)
	if len(got) != 2 {
		t.Fatalf("expected plans up to tier 20, got %d", len(got))
	}
}

// The guard's whole purpose: Kubernetes does not evict pods to fit a shrunken
// quota, it refuses the next create — so a downgrade below current use fails
// silently, hours later, at the next restart.
func TestCheckFitRefusesADowngradeBelowCurrentUse(t *testing.T) {
	p := plan("small", 0, "8", "16Gi")
	used := corev1.ResourceList{
		corev1.ResourceLimitsCPU:    resource.MustParse("12"),
		corev1.ResourceLimitsMemory: resource.MustParse("8Gi"),
	}

	err := CheckFit(&p, used, 0)
	if err == nil {
		t.Fatal("expected the downgrade to be refused")
	}
	var downgrade *DowngradeError
	if !errors.As(err, &downgrade) {
		t.Fatalf("expected a DowngradeError, got %T", err)
	}
	if len(downgrade.Shortfalls) != 1 {
		t.Fatalf("expected only CPU to be short, got %v", downgrade.Shortfalls)
	}
	if downgrade.Shortfalls[0].Resource != string(corev1.ResourceLimitsCPU) {
		t.Fatalf("expected limits.cpu, got %s", downgrade.Shortfalls[0].Resource)
	}
}

func TestCheckFitAllowsAPlanThatExactlyFits(t *testing.T) {
	p := plan("exact", 0, "12", "8Gi")
	used := corev1.ResourceList{
		corev1.ResourceLimitsCPU:    resource.MustParse("12"),
		corev1.ResourceLimitsMemory: resource.MustParse("8Gi"),
	}
	if err := CheckFit(&p, used, 0); err != nil {
		t.Fatalf("a plan equal to current use must be allowed: %v", err)
	}
}

// maxApps is enforced by the Tenant webhook against spec.apps, not by the
// ResourceQuota, so it has to be checked separately or a tenant discovers it at
// the next unrelated edit to their tenant.yaml.
func TestCheckFitCountsInstalledAppsAgainstMaxApps(t *testing.T) {
	p := plan("small", 0, "64", "64Gi")
	p.Spec.Quotas.MaxApps = 3

	if err := CheckFit(&p, nil, 3); err != nil {
		t.Fatalf("exactly at the app ceiling must be allowed: %v", err)
	}
	err := CheckFit(&p, nil, 4)
	if err == nil {
		t.Fatal("expected more apps than the plan allows to be refused")
	}
}

func TestDescribePairsUsedAndHardWithARatio(t *testing.T) {
	quota := &corev1.ResourceQuota{
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("32")},
			Used: corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("8")},
		},
	}
	rows := Describe(quota)
	if len(rows) != 1 {
		t.Fatalf("expected one row, got %d", len(rows))
	}
	if rows[0].UsedRatio == nil || *rows[0].UsedRatio != 0.25 {
		t.Fatalf("expected a ratio of 0.25, got %v", rows[0].UsedRatio)
	}
}

// A resource with no ceiling gets no ratio: reporting 0 would draw a full-empty
// bar for something that is in fact unlimited.
func TestDescribeOmitsTheRatioWhenThereIsNoCeiling(t *testing.T) {
	quota := &corev1.ResourceQuota{
		Status: corev1.ResourceQuotaStatus{
			Used: corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("8")},
		},
	}
	rows := Describe(quota)
	if len(rows) != 1 || rows[0].UsedRatio != nil {
		t.Fatalf("expected no ratio without a hard limit, got %v", rows)
	}
}
