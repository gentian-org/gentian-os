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

func profile(name, family, role string, addon *gentianov1alpha1.CustomizationAddon, license string) *gentianov1alpha1.AppProfile {
	p := &gentianov1alpha1.AppProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if role != "" {
		p.Annotations = map[string]string{gentianov1alpha1.AnnotationProfileDeploymentRole: role}
	}
	p.Spec.Family = family
	p.Spec.License = license
	if addon != nil {
		p.Spec.Customization = &gentianov1alpha1.CustomizationSurface{Addon: addon}
	}
	return p
}

func odooFixture() (*gentianov1alpha1.AppProfile, map[string]*gentianov1alpha1.AppProfile) {
	base := profile("odoo-base-ce", "odoo", "base", nil, "LGPL-3.0")
	idx := map[string]*gentianov1alpha1.AppProfile{
		"odoo-base-ce": base,
		"odoo-crm-ce": profile("odoo-crm-ce", "odoo", "addon",
			&gentianov1alpha1.CustomizationAddon{ID: "crm", Of: "odoo-base-ce"}, "LGPL-3.0"),
		"odoo-accounting-ce": profile("odoo-accounting-ce", "odoo", "addon",
			&gentianov1alpha1.CustomizationAddon{ID: "account", Of: "odoo-base-ce"}, "LGPL-3.0"),
	}
	return base, idx
}

func TestResolveAddonsMapsToAppSideIDs(t *testing.T) {
	base, idx := odooFixture()
	got, errs := ResolveAddons(base, []string{"odoo-crm-ce", "odoo-accounting-ce"}, idx)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// sorted by profile name: accounting before crm
	want := []string{"account", "crm"}
	if ids := AddonIDs(got); len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids: got %v, want %v", ids, want)
	}
}

func TestResolveAddonsDeduplicatesSelection(t *testing.T) {
	base, idx := odooFixture()
	got, errs := ResolveAddons(base, []string{"odoo-crm-ce", "odoo-crm-ce", ""}, idx)
	if len(errs) != 0 || len(got) != 1 {
		t.Fatalf("got %d addon(s), errs %v", len(got), errs)
	}
}

func TestResolveAddonsRejectsNonAddonProfile(t *testing.T) {
	base, idx := odooFixture()
	// selecting the base itself, or any standalone app, is not an addon selection
	_, errs := ResolveAddons(base, []string{"odoo-base-ce"}, idx)
	if !anyContains(errs, "not addon") {
		t.Fatalf("expected role rejection, got %v", errs)
	}
}

func TestResolveAddonsRejectsUndeclaredAddon(t *testing.T) {
	base, idx := odooFixture()
	idx["odoo-broken-ce"] = profile("odoo-broken-ce", "odoo", "addon", nil, "LGPL-3.0")
	_, errs := ResolveAddons(base, []string{"odoo-broken-ce"}, idx)
	if !anyContains(errs, "spec.customization.addon is not declared") {
		t.Fatalf("expected missing-declaration error, got %v", errs)
	}
}

func TestResolveAddonsRejectsWrongBaseAndFamily(t *testing.T) {
	base, idx := odooFixture()
	idx["nextcloud-mail-ce"] = profile("nextcloud-mail-ce", "nextcloud", "addon",
		&gentianov1alpha1.CustomizationAddon{ID: "mail", Of: "nextcloud-base-ce"}, "AGPL-3.0-only")
	_, errs := ResolveAddons(base, []string{"nextcloud-mail-ce"}, idx)
	if !anyContains(errs, "activates into") {
		t.Fatalf("expected wrong-base rejection, got %v", errs)
	}
}

func TestResolveAddonsRejectsDuplicateAppSideID(t *testing.T) {
	base, idx := odooFixture()
	// a pro edition of the same addon resolves to the same Odoo module
	idx["odoo-crm-pro"] = profile("odoo-crm-pro", "odoo", "addon",
		&gentianov1alpha1.CustomizationAddon{ID: "crm", Of: "odoo-base-ce"}, "proprietary")
	_, errs := ResolveAddons(base, []string{"odoo-crm-ce", "odoo-crm-pro"}, idx)
	if !anyContains(errs, "both resolve to id") {
		t.Fatalf("expected duplicate-id rejection, got %v", errs)
	}
}

func TestResolveAddonsReportsEveryProblem(t *testing.T) {
	base, idx := odooFixture()
	_, errs := ResolveAddons(base, []string{"ghost-ce", "odoo-base-ce"}, idx)
	if len(errs) != 2 {
		t.Fatalf("expected both problems reported, got %v", errs)
	}
}

func TestEntitledAddonsBlocksUngrantedCommercial(t *testing.T) {
	base, idx := odooFixture()
	idx["odoo-payroll-pro"] = profile("odoo-payroll-pro", "odoo", "addon",
		&gentianov1alpha1.CustomizationAddon{ID: "payroll", Of: "odoo-base-ce"}, "proprietary")
	resolved, errs := ResolveAddons(base, []string{"odoo-crm-ce", "odoo-payroll-pro"}, idx)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	allowed, blocked := EntitledAddons(resolved, map[string]bool{})
	if len(allowed) != 1 || allowed[0].ID != "crm" {
		t.Fatalf("allowed: %+v", allowed)
	}
	if len(blocked) != 1 || blocked[0].Profile != "odoo-payroll-pro" {
		t.Fatalf("blocked: %+v", blocked)
	}
	// granting the entitlement unblocks it — compatibility never was the gate
	allowed, blocked = EntitledAddons(resolved, map[string]bool{"odoo-payroll-pro": true})
	if len(allowed) != 2 || len(blocked) != 0 {
		t.Fatalf("after grant: allowed=%d blocked=%d", len(allowed), len(blocked))
	}
}

func anyContains(errs []error, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}
