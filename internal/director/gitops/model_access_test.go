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
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

const claimWithProviders = `  llm:
    enabled: true
    providers:
      - name: acme
        apiBase: https://api.acme.example/v1
        apiKeyProperty: acme_api_key
        models: []
      - name: open-one
        apiBase: https://open.example/v1
        apiKeyProperty: open_one_api_key
        models: []
`

func modelAccessOf(t *testing.T, g *gitops.GitOps) (*gentianov1alpha1.TenantLLM, string) {
	t.Helper()
	file, err := g.TenantFile(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var tn gentianov1alpha1.Tenant
	if err := yaml.UnmarshalStrict(raw, &tn); err != nil {
		t.Fatalf("the manifest is no Tenant: %v\n%s", err, raw)
	}
	return tn.Spec.LLM, string(raw)
}

// What a tenant's apps may use at the model gateway is spec.llm of its
// manifest: written, read back, changed and removed, each a commit, with the
// rest of the manifest as it was.
func TestATenantsModelAccessIsDeclaredInItsManifest(t *testing.T) {
	g := claimWith(t, claimWithProviders)
	ctx := context.Background()

	if got, err := g.TenantModelAccess(ctx, "demo"); err != nil || got != nil {
		t.Fatalf("before: %+v, %v", got, err)
	}
	_, before := modelAccessOf(t, g)

	budget, rpm := int64(40), int64(30)
	want := &gentianov1alpha1.TenantLLM{
		Providers: []string{"acme", "acme"},
		Budget:    &gentianov1alpha1.TenantLLMBudget{MaxBudget: &budget, BudgetDuration: "30d", RPMLimit: &rpm},
	}
	if res, err := g.SetTenantModelAccess(ctx, "demo", want, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("set: %+v, %v", res, err)
	}
	got, text := modelAccessOf(t, g)
	if got == nil || strings.Join(got.Providers, ",") != "acme" || *got.Budget.MaxBudget != 40 ||
		got.Budget.BudgetDuration != "30d" || *got.Budget.RPMLimit != 30 || got.Budget.TPMLimit != nil {
		t.Fatalf("spec.llm = %+v\n%s", got, text)
	}
	if read, err := g.TenantModelAccess(ctx, "demo"); err != nil || read == nil || read.Providers[0] != "acme" {
		t.Fatalf("read back: %+v, %v", read, err)
	}
	if res, err := g.SetTenantModelAccess(ctx, "demo", want, tenantMeta()); err != nil || res.Changed {
		t.Fatalf("the same again is no commit: %+v, %v", res, err)
	}

	// The budget alone.
	if _, err := g.SetTenantModelAccess(ctx, "demo", &gentianov1alpha1.TenantLLM{Budget: want.Budget}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if got, _ := modelAccessOf(t, g); got == nil || got.Providers != nil || got.Budget == nil {
		t.Fatalf("budget alone: %+v", got)
	}

	// Nothing: spec.llm goes, and the manifest is what it was.
	if _, err := g.SetTenantModelAccess(ctx, "demo", &gentianov1alpha1.TenantLLM{}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if got, text := modelAccessOf(t, g); got != nil || text != before {
		t.Fatalf("cleared: %+v\n%s", got, text)
	}
}

// A provider the claim does not list, a name that is not one, or a budget
// the schema refuses is not written.
func TestModelAccessTheClusterDoesNotOfferIsRefused(t *testing.T) {
	g := claimWith(t, claimWithProviders)
	ctx := context.Background()
	zero := int64(0)
	for name, access := range map[string]*gentianov1alpha1.TenantLLM{
		"unknown provider": {Providers: []string{"elsewhere"}},
		"not a name":       {Providers: []string{"Acme/x"}},
		"period":           {Budget: &gentianov1alpha1.TenantLLMBudget{BudgetDuration: "a month"}},
		"rpm":              {Budget: &gentianov1alpha1.TenantLLMBudget{RPMLimit: &zero}},
	} {
		if _, err := g.SetTenantModelAccess(ctx, "demo", access, tenantMeta()); !errors.Is(err, gitops.ErrInvalidModelAccess) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if got, _ := modelAccessOf(t, g); got != nil {
		t.Errorf("something was written: %+v", got)
	}
}
