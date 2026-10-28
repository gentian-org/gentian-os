/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Who a tenant is lands in its manifest as spec.organization beside a new
// display name, reads back with what is still missing, takes the country's
// currency when it names none, and is removed again when emptied.
func TestATenantsOrganizationIsDeclaredInItsManifest(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme", DisplayName: "Acme", Country: "CH"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	got, err := g.TenantOrganization(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"organization.legalName", "organization.currency", "organization.timeZone", "locales"}; !reflect.DeepEqual(got.Missing, want) {
		t.Fatalf("missing = %v for a new tenant, want %v", got.Missing, want)
	}

	org := gentianov1alpha1.TenantOrganization{
		LegalName: " Acme AG ",
		TimeZone:  "Europe/Zurich",
		Address:   &gentianov1alpha1.TenantAddress{Street: "Bahnhofstrasse 1", PostalCode: "8001", City: "Zürich", Region: "ZH"},
		Website:   "https://acme.example",
		Email:     "info@acme.example",
		TaxID:     "CHE-123.456.789 MWST",
	}
	if res, err := g.SetTenantOrganization(ctx, "acme", "Acme Corp", org, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("set: %+v, %v", res, err)
	}
	var tn gentianov1alpha1.Tenant
	if err := yaml.Unmarshal([]byte(dt.RemoteFile(t, remote, dt.TenantPath("acme"))), &tn); err != nil {
		t.Fatal(err)
	}
	if tn.Spec.DisplayName != "Acme Corp" || tn.Spec.Country != "CH" || tn.Spec.Organization == nil {
		t.Fatalf("spec = %+v", tn.Spec)
	}
	if o := tn.Spec.Organization; o.LegalName != "Acme AG" || o.Currency != "CHF" || o.Address.City != "Zürich" {
		t.Fatalf("organization = %+v, want the name trimmed and the country's currency", o)
	}
	if res, err := g.SetTenantOrganization(ctx, "acme", "", org, tenantMeta()); err != nil || res.Changed {
		t.Fatalf("the same organisation again is no commit: %+v, %v", res, err)
	}
	if _, err := g.SetTenantLocales(ctx, "acme", []string{"de", "en"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if got, err = g.TenantOrganization(ctx, "acme"); err != nil || len(got.Missing) != 0 || got.DisplayName != "Acme Corp" {
		t.Fatalf("complete tenant: %+v, %v", got, err)
	}

	for field, bad := range map[string]gentianov1alpha1.TenantOrganization{
		"currency": {Currency: "XYZ"},
		"timeZone": {TimeZone: "Mars/Olympus"},
		"website":  {Website: "acme.example"},
		"email":    {Email: "Acme <info@acme.example>"},
		"month":    {FiscalYearStartMonth: 13},
	} {
		if _, err := g.SetTenantOrganization(ctx, "acme", "", bad, tenantMeta()); !errors.Is(err, gitops.ErrInvalidOrganization) {
			t.Errorf("%s: err = %v", field, err)
		}
	}

	if _, err := g.SetTenantCountry(ctx, "acme", "", tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.SetTenantOrganization(ctx, "acme", "", gentianov1alpha1.TenantOrganization{}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if text := dt.RemoteFile(t, remote, dt.TenantPath("acme")); strings.Contains(text, "organization") || !strings.Contains(text, "Acme Corp") {
		t.Errorf("an emptied organisation is still in the manifest, or the name went with it:\n%s", text)
	}
}

func TestCountryCurrency(t *testing.T) {
	for country, want := range map[string]string{"CH": "CHF", "DE": "EUR", "US": "USD", "": "", "ZZ": ""} {
		if got := gitops.CountryCurrency(country); got != want {
			t.Errorf("CountryCurrency(%q) = %q, want %q", country, got, want)
		}
	}
}
