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
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// A tenant's country is stated when it is made, read back, changed and
// cleared, each a commit of spec.country; a manifest without one names none.
func TestATenantsCountryIsDeclaredInItsManifest(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme", DisplayName: "Acme", Country: "ch"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	read := func() gentianov1alpha1.Tenant {
		t.Helper()
		var tn gentianov1alpha1.Tenant
		if err := yaml.Unmarshal([]byte(dt.RemoteFile(t, remote, dt.TenantPath("acme"))), &tn); err != nil {
			t.Fatal(err)
		}
		return tn
	}
	if got := read().Spec.Country; got != "CH" {
		t.Fatalf("spec.country = %q after create, want CH", got)
	}
	if got, err := g.TenantCountry(ctx, "acme"); err != nil || got != "CH" {
		t.Fatalf("TenantCountry = %q, %v", got, err)
	}
	tenants, err := g.TenantDetails(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tn := range tenants {
		if tn.Name == "acme" && tn.Country != "CH" {
			t.Errorf("the listing does not say the country: %+v", tn)
		}
	}

	// NO is Norway, not YAML's false.
	if res, err := g.SetTenantCountry(ctx, "acme", "NO", tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("set NO: %+v, %v", res, err)
	}
	if got := read().Spec.Country; got != "NO" {
		t.Fatalf("spec.country = %q, want NO", got)
	}
	if res, err := g.SetTenantCountry(ctx, "acme", "no", tenantMeta()); err != nil || res.Changed {
		t.Fatalf("the same country again is no commit: %+v, %v", res, err)
	}
	if _, err := g.SetTenantCountry(ctx, "acme", "", tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if text := dt.RemoteFile(t, remote, dt.TenantPath("acme")); strings.Contains(text, "country") {
		t.Errorf("a cleared country is still in the manifest:\n%s", text)
	}

	if _, err := g.SetTenantCountry(ctx, "acme", "CHE", tenantMeta()); !errors.Is(err, gitops.ErrInvalidCountry) {
		t.Errorf("an alpha-3 code: err = %v", err)
	}
	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "globex", Country: "Schweiz"}, tenantMeta()); !errors.Is(err, gitops.ErrInvalidCountry) {
		t.Errorf("create with a country that is no code: err = %v", err)
	}

	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "initech"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if text := dt.RemoteFile(t, remote, dt.TenantPath("initech")); strings.Contains(text, "country") {
		t.Errorf("a tenant made without a country names one:\n%s", text)
	}
}
