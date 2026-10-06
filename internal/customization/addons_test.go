/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package customization

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func profile(name, family, role string, addon *gentianov1alpha1.CustomizationAddon, license string) *gentianov1alpha1.ComponentProfile {
	p := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if role != "" {
		p.Annotations = map[string]string{gentianov1alpha1.AnnotationProfileDeploymentRole: role}
	}
	// family and license are the store's now (AD-3). What is left in the
	// cluster is the addon declaration itself.
	_, _ = family, license
	if addon != nil {
		p.Spec.Package.Addon = &gentianov1alpha1.PackageAddon{ID: addon.ID, Of: addon.Of}
	}
	return p
}

func odooFixture() (*gentianov1alpha1.ComponentProfile, map[string]*gentianov1alpha1.ComponentProfile) {
	base := profile("odoo-base-ce", "odoo", "base", nil, "LGPL-3.0")
	idx := map[string]*gentianov1alpha1.ComponentProfile{
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

// Selecting something that is not an addon is one refusal, not two.
//
// It used to be two: an annotation said whether a profile WAS an addon, and
// spec.customization.addon said what the app called it and which base it
// activated into — so a profile could claim to be an addon and then not say
// what it was, and the resolver had a separate error for each. AD-4 leaves one
// statement, package.addon, and it carries both facts. A profile without it is
// not an addon; there is nothing else to be inconsistent with.
func TestResolveAddonsRejectsAnythingThatIsNotAnAddon(t *testing.T) {
	for name, selected := range map[string]string{
		// The base itself, or any standalone app.
		"a standalone app":      "odoo-base-ce",
		"a profile claiming to": "odoo-broken-ce",
	} {
		base, idx := odooFixture()
		idx["odoo-broken-ce"] = profile("odoo-broken-ce", "odoo", "addon", nil, "LGPL-3.0")
		_, errs := ResolveAddons(base, []string{selected}, idx)
		if !anyContains(errs, "package.addon is not declared") {
			t.Errorf("%s: expected a not-an-addon refusal, got %v", name, errs)
		}
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
	// an ee edition of the same addon resolves to the same Odoo module
	idx["odoo-crm-ee"] = profile("odoo-crm-ee", "odoo", "addon",
		&gentianov1alpha1.CustomizationAddon{ID: "crm", Of: "odoo-base-ce"}, "proprietary")
	_, errs := ResolveAddons(base, []string{"odoo-crm-ce", "odoo-crm-ee"}, idx)
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

// A paid addon resolves and is activated like any other: the platform gates
// none, and whether it arrives is decided at the repository it is pulled from.
func TestResolveAddonsTreatsAPaidAddonLikeAnyOther(t *testing.T) {
	base, idx := odooFixture()
	idx["odoo-payroll-ee"] = profile("odoo-payroll-ee", "odoo", "addon",
		&gentianov1alpha1.CustomizationAddon{ID: "payroll", Of: "odoo-base-ce"}, "proprietary")
	resolved, errs := ResolveAddons(base, []string{"odoo-crm-ce", "odoo-payroll-ee"}, idx)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if ids := AddonIDs(resolved); len(ids) != 2 || ids[0] != "crm" || ids[1] != "payroll" {
		t.Fatalf("activated ids = %v, want [crm payroll]", ids)
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
