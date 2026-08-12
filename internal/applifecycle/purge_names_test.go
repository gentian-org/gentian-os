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
	"testing"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The database and the login role are named by different rules: the database
// has hyphens replaced to satisfy PostgreSQL identifier limits, the role keeps
// them. Purge previously used one name for both, so DROP DATABASE matched and
// DROP ROLE silently did not -- every purged app left its role behind. These
// assert the asymmetry directly, because it is the kind of detail a later
// "tidy-up" would collapse back into a single helper.
func TestPostgresNamesDifferForHyphenatedApps(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}

	if got, want := databaseName(tenant, "docmost-ce"), "demo_docmost_ce"; got != want {
		t.Errorf("databaseName = %q, want %q", got, want)
	}
	if got, want := pgRoleName(tenant.Name, "docmost-ce"), "demo_docmost-ce"; got != want {
		t.Errorf("pgRoleName = %q, want %q", got, want)
	}
	if databaseName(tenant, "docmost-ce") == pgRoleName(tenant.Name, "docmost-ce") {
		t.Error("database and role names must not be equal for a hyphenated app; " +
			"purge relies on dropping each by its own name")
	}
}

// An app without hyphens names both the same, which is why the bug went
// unnoticed: those apps purged completely.
func TestPostgresNamesAgreeWithoutHyphens(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}

	if got, want := databaseName(tenant, "xwiki"), "demo_xwiki"; got != want {
		t.Errorf("databaseName = %q, want %q", got, want)
	}
	if got, want := pgRoleName(tenant.Name, "xwiki"), "demo_xwiki"; got != want {
		t.Errorf("pgRoleName = %q, want %q", got, want)
	}
}

// The custom prefix applies to the database only; the role is always
// "<tenant>_<app>", so a tenant with a prefix must not shift the role name.
func TestDatabasePrefixDoesNotChangeRoleName(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Isolation: &gentianov1alpha1.TenantIsolation{DatabasePrefix: "custom_"},
		},
	}

	if got, want := databaseName(tenant, "docmost-ce"), "custom_docmost_ce"; got != want {
		t.Errorf("databaseName = %q, want %q", got, want)
	}
	if got, want := pgRoleName(tenant.Name, "docmost-ce"), "demo_docmost-ce"; got != want {
		t.Errorf("pgRoleName = %q, want %q", got, want)
	}
}
