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
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// The repository a catalogue artifact is read with is one its owner
// declared, and nobody else's: a tenant's own, or the cluster's. Every other
// name is answered as one nobody declared.
func TestARepositoryIsAnsweredToItsOwnerOnly(t *testing.T) {
	remote := dt.Remote(t, "demo", "other")
	seedClaim(t, remote, "ours-repository.yaml", `apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: ours
spec:
  type: oci
  role: apps
  endpoints:
    inCluster: oci://registry.example.com/ours
  credential:
    vaultPath: gentian-os/kernel/repositories/ours
    displayName: ours
`)
	seedClaim(t, remote, "elsewhere-repository.yaml", `apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: elsewhere
spec:
  type: oci
  role: apps
  endpoints:
    inCluster: oci://registry.example.com/elsewhere
  credential:
    vaultPath: gentian-os/tenants/demo/repositories/acme
    displayName: a path of a tenant's
`)
	seedClaim(t, remote, "deployments-repository.yaml", installerDeployments)
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	for _, d := range []struct{ tenant, name, url string }{
		{"demo", "acme", "oci://ghcr.io/acme"},
		{"other", "theirs", "oci://ghcr.io/theirs"},
	} {
		if _, err := g.DeclareRepository(ctx, d.tenant, d.name, gitops.RepositoryDeclaration{Role: "apps", Type: "oci", URL: d.url}, repoMeta()); err != nil {
			t.Fatal(err)
		}
	}

	got, err := g.RegistryRepository(ctx, "demo", "acme")
	if err != nil || got != (catalogue.DeclaredRegistry{URL: "oci://ghcr.io/acme", VaultPath: "gentian-os/tenants/demo/repositories/acme"}) {
		t.Fatalf("demo's own: %+v %v", got, err)
	}
	if got.VaultPath != catalogue.RepositoryCredentialPrefix("demo")+"acme" || gitops.RepositoryVaultPath("", "x") != catalogue.RepositoryCredentialPrefix("")+"x" {
		t.Fatal("the director reads repository credentials somewhere other than where they are declared")
	}
	got, err = g.RegistryRepository(ctx, "", "ours")
	if err != nil || got.VaultPath != "gentian-os/kernel/repositories/ours" {
		t.Fatalf("the cluster's own: %+v %v", got, err)
	}
	for _, c := range []struct{ owner, name string }{
		{"demo", "theirs"}, // another tenant's
		{"other", "acme"},  // and the other way round
		{"demo", "ours"},   // the cluster's, for a tenant
		{"", "acme"},       // a tenant's, for the cluster
		{"demo", "absent"},
		{"", "absent"},
		{"ghost", "acme"},
	} {
		if _, err := g.RegistryRepository(ctx, c.owner, c.name); !errors.Is(err, gitops.ErrRepositoryNotFound) {
			t.Errorf("%q asking for %s: %v, want not found", c.owner, c.name, err)
		}
	}
	if _, err := g.RegistryRepository(ctx, "", "deployments"); !errors.Is(err, gitops.ErrRepositoryNotRegistry) {
		t.Errorf("a git repository: %v", err)
	}
	if _, err := g.RegistryRepository(ctx, "", "elsewhere"); !errors.Is(err, catalogue.ErrRegistryRefused) {
		t.Errorf("a credential declared at another owner's path: %v", err)
	}
}

// An artifact is added for a tenant by the cluster's administrator, or by
// the tenant's own where that is delegated, as an address is.
func TestAnArtifactIsAddedAsAnAddressIs(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	oci := gitops.CatalogueOCI{Repository: "acme", Artifact: "ghcr.io/acme/catalogue:1"}
	if _, err := g.AddTenantCatalogueOCI(ctx, "demo", "acme", oci, gitops.ByTenant, repoMeta()); !errors.Is(err, gitops.ErrCatalogueNotDelegated) {
		t.Fatalf("not delegated: %v", err)
	}
	if _, err := g.SetTenantCatalogueDelegation(ctx, "demo", true, repoMeta()); err != nil {
		t.Fatal(err)
	}
	if res, err := g.AddTenantCatalogueOCI(ctx, "demo", "acme", oci, gitops.ByTenant, repoMeta()); err != nil || !res.Changed {
		t.Fatalf("delegated: %+v %v", res, err)
	}
	if res, err := g.AddTenantCatalogueOCI(ctx, "demo", "acme", oci, gitops.ByTenant, repoMeta()); err != nil || res.Status != "unchanged" {
		t.Fatalf("again: %+v %v", res, err)
	}
	c, err := g.TenantCatalogue(ctx, "demo")
	if err != nil || len(c.Sources) != 1 || c.Sources[0].OCI != oci || c.Sources[0].AddedBy != "tenant" {
		t.Fatalf("read back %+v %v", c, err)
	}
	if _, err := g.AddTenantCatalogueOCI(ctx, "demo", "bad", gitops.CatalogueOCI{Repository: "acme", Artifact: "10.0.0.1/x/y:1"}, gitops.ByCluster, repoMeta()); err == nil {
		t.Fatal("an artifact on a private address was written")
	}
	if _, err := g.AddClusterCatalogueOCI(ctx, "acme", oci, repoMeta()); !errors.Is(err, gitops.ErrCatalogueNameTaken) {
		t.Fatalf("the cluster taking a tenant's name: %v", err)
	}
	if res, err := g.AddClusterCatalogueOCI(ctx, "ours", gitops.CatalogueOCI{Repository: "ours", Artifact: "ghcr.io/ours/catalogue:1"}, repoMeta()); err != nil || !res.Changed {
		t.Fatalf("the cluster's: %+v %v", res, err)
	}
	cluster, err := g.Catalogue(ctx)
	if err != nil || len(cluster.Sources) != 1 || cluster.Sources[0].OCI.Repository != "ours" {
		t.Fatalf("the claim reads %+v %v", cluster, err)
	}
}
