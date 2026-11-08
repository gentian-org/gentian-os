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

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// A catalog renamed in place: the source entry, every app entry and add-on
// pin that names it, and the origin of every profile, bundle and build that
// came from it, as one commit. What came from another catalog, and the
// bytes every pin's digest was taken over, are not touched.

const renameClaim = "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: " + dt.Cluster +
	"\nspec:\n  kernelDomain: " + dt.KernelDomain + "\n  catalogue:\n    storeUrl: https://store.example.com\n    sources:\n" +
	"      - name: main\n        url: https://store.example.com/catalogue\n" +
	"      - name: partner\n        url: https://partner.example.com/apps\n        addedBy: store\n"

// soloManifest has an app and an add-on pinned from partner, an app from
// main, and a chart value of the same name, which is the chart's.
const soloManifest = `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: solo
spec:
  displayName: solo
  apps:
  - profile: shop
    digest: sha256:` + "1111111111111111111111111111111111111111111111111111111111111111" + `
    catalogue: partner
    addons:
    - shop-pay
    addonPins:
    - name: shop-pay
      digest: sha256:` + "2222222222222222222222222222222222222222222222222222222222222222" + `
      catalogue: partner
    config:
      catalogue: partner
  - profile: notes
    digest: sha256:` + "3333333333333333333333333333333333333333333333333333333333333333" + `
    catalogue: main
`

func materialise(t *testing.T, g *gitops.GitOps, name, origin string) string {
	t.Helper()
	body := []byte(dt.ProfileYAML(name))
	digest := profilebundle.Digest(body)
	if _, err := g.MaterialiseProfile(context.Background(), name, digest, body, origin, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestRenamingAClusterCatalogueRewritesEverythingThatNamesIt(t *testing.T) {
	remote := dt.Remote(t, "demo", "solo")
	dt.Commit(t, remote, map[string]string{
		dt.ClaimPath:          renameClaim,
		dt.TenantPath("solo"): soloManifest,
	})
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	shop := materialise(t, g, "shop", profilebundle.ClusterOrigin("partner"))
	notes := materialise(t, g, "notes", profilebundle.ClusterOrigin("main"))
	if _, err := g.InstallFrom(ctx, "demo", "shop", shop, "partner", nil, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, path := range []string{
		dt.CataloguePath("shop.yaml"), dt.CataloguePath("notes.yaml"), dt.CataloguePath(gitops.BundleFile("notes")),
		dt.CataloguePath(gitops.BuildsDir + "/" + gitops.BuildFile("notes", notes)), dt.CataloguePath("kustomization.yaml"),
	} {
		before[path] = dt.RemoteFile(t, remote, path)
	}
	tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	res, err := g.RenameCatalogueSource(ctx, "", "partner", "acme", meta("u-ada"))
	if err != nil || res.Status != "renamed" || !res.Changed || res.Commit == "" {
		t.Fatalf("rename = %+v, %v", res, err)
	}
	// One commit, by the person, saying what it did.
	if parent := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main~1"); parent != tip {
		t.Fatal("the rename is more than one commit")
	}
	if subject := dt.Git(t, "", "--git-dir", remote, "log", "-1", "--format=%an|%s", "main"); !strings.Contains(subject, "Ada Lovelace|feat(cluster): rename catalog partner to acme (via ada@example.com)") {
		t.Fatalf("commit = %q", subject)
	}

	// The claim: the entry renamed in place, its address and its mark kept,
	// the other entry as it was.
	claim := dt.RemoteFile(t, remote, dt.ClaimPath)
	want := "    sources:\n      - name: main\n        url: https://store.example.com/catalogue\n" +
		"      - name: acme\n        url: https://partner.example.com/apps\n        addedBy: store"
	if !strings.Contains(claim, want) || strings.Contains(claim, "name: partner") {
		t.Fatalf("the claim:\n%s", claim)
	}
	// Every tenant's entries from it, and its pins: not the chart's value,
	// and not an entry from another catalog.
	demo := dt.RemoteFile(t, remote, dt.TenantPath("demo"))
	if !strings.Contains(demo, "  - profile: shop\n    digest: "+shop+"\n    catalogue: acme\n") {
		t.Fatalf("demo's entry:\n%s", demo)
	}
	solo := dt.RemoteFile(t, remote, dt.TenantPath("solo"))
	wantSolo := strings.NewReplacer(
		"    catalogue: partner\n    addons:", "    catalogue: acme\n    addons:",
		"      catalogue: partner\n    config:", "      catalogue: acme\n    config:",
	).Replace(soloManifest)
	if solo != strings.TrimSpace(wantSolo) {
		t.Fatalf("solo's manifest:\n%s", solo)
	}
	// The profile's origin, on the bundle and on the build; the bytes the
	// digest was taken over, and the kustomization, as they were.
	bundle := dt.RemoteFile(t, remote, dt.CataloguePath(gitops.BundleFile("shop")))
	if !strings.Contains(bundle, profilebundle.OriginAnnotation+": cluster/acme") || strings.Contains(bundle, "partner") {
		t.Fatalf("shop's bundle:\n%s", bundle)
	}
	build := dt.RemoteFile(t, remote, dt.CataloguePath(gitops.BuildsDir+"/"+gitops.BuildFile("shop", shop)))
	if !strings.Contains(build, "\n  origin: cluster/acme\n") || strings.Contains(build, "partner") {
		t.Fatalf("shop's build:\n%s", build)
	}
	for path, was := range before {
		if got := dt.RemoteFile(t, remote, path); got != was {
			t.Errorf("%s changed:\n%s", path, got)
		}
	}
	have, err := g.ProfileOnCluster(ctx, "shop")
	if err != nil || have.Origin != "cluster/acme" || have.Digest != shop {
		t.Fatalf("shop on the cluster = %+v, %v", have, err)
	}
	// The next install from the renamed catalog is the same origin again:
	// the same bytes, nothing to write.
	body := []byte(dt.ProfileYAML("shop"))
	if res, err := g.MaterialiseProfile(ctx, "shop", shop, body, profilebundle.ClusterOrigin("acme"), meta("u-ada")); err != nil || res.Status != "unchanged" {
		t.Fatalf("materialize from acme = %+v, %v", res, err)
	}
}

func TestRenamingATenantsCatalogueTouchesThatTenantOnly(t *testing.T) {
	remote := dt.Remote(t, "demo", "solo")
	dt.Commit(t, remote, map[string]string{dt.ClaimPath: renameClaim})
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	// Two tenants with a catalog of the same name, as two tenants may.
	for _, tenant := range []string{"demo", "solo"} {
		if _, err := g.AddTenantCatalogueSource(ctx, tenant, "aluvian", "https://"+tenant+".example.com/apps", gitops.ByCluster, meta("u-ada")); err != nil {
			t.Fatal(err)
		}
	}
	crm := materialise(t, g, "crm", profilebundle.TenantOrigin("demo", "aluvian"))
	erp := materialise(t, g, "erp", profilebundle.TenantOrigin("solo", "aluvian"))
	if _, err := g.InstallFrom(ctx, "demo", "crm", crm, "aluvian", nil, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.InstallFrom(ctx, "solo", "erp", erp, "aluvian", nil, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	untouched := map[string]string{}
	for _, path := range []string{
		dt.ClaimPath, dt.TenantPath("solo"), dt.CataloguePath("crm.yaml"), dt.CataloguePath("erp.yaml"),
		dt.CataloguePath(gitops.BundleFile("erp")), dt.CataloguePath(gitops.BuildsDir + "/" + gitops.BuildFile("erp", erp)),
	} {
		untouched[path] = dt.RemoteFile(t, remote, path)
	}

	res, err := g.RenameCatalogueSource(ctx, "demo", "aluvian", "aluvian-team", meta("u-ada"))
	if err != nil || res.Status != "renamed" || !res.Changed {
		t.Fatalf("rename = %+v, %v", res, err)
	}
	if subject := dt.Git(t, "", "--git-dir", remote, "log", "-1", "--format=%s", "main"); subject != "feat(demo): rename catalog aluvian to aluvian-team (via ada@example.com)" {
		t.Fatalf("commit = %q", subject)
	}
	demo := dt.RemoteFile(t, remote, dt.TenantPath("demo"))
	if !strings.Contains(demo, "  catalogue:\n    sources:\n      - name: aluvian-team\n        url: https://demo.example.com/apps\n        addedBy: cluster") {
		t.Fatalf("demo's sources:\n%s", demo)
	}
	if !strings.Contains(demo, "    catalogue: aluvian-team\n") || strings.Contains(demo, "catalogue: aluvian\n") {
		t.Fatalf("demo's entry:\n%s", demo)
	}
	bundle := dt.RemoteFile(t, remote, dt.CataloguePath(gitops.BundleFile("crm")))
	if !strings.Contains(bundle, profilebundle.OriginAnnotation+": tenant/demo/aluvian-team") {
		t.Fatalf("crm's bundle:\n%s", bundle)
	}
	build := dt.RemoteFile(t, remote, dt.CataloguePath(gitops.BuildsDir+"/"+gitops.BuildFile("crm", crm)))
	if !strings.Contains(build, "\n  origin: tenant/demo/aluvian-team\n") {
		t.Fatalf("crm's build:\n%s", build)
	}
	for path, was := range untouched {
		if got := dt.RemoteFile(t, remote, path); got != was {
			t.Errorf("%s changed:\n%s", path, got)
		}
	}
	own, err := g.TenantCatalogue(ctx, "demo")
	if err != nil || len(own.Sources) != 1 || own.Sources[0].Name != "aluvian-team" || own.Sources[0].AddedBy != "cluster" {
		t.Fatalf("demo's catalog = %+v, %v", own, err)
	}
}

// Refused, and nothing committed: a name that is not there, a name that is
// taken -- in the same scope, by the cluster for a tenant's, by any tenant
// for the cluster's -- and a name that is not a name. The same name again
// is not a change.
func TestARenameIsRefusedWhereAddingWouldBe(t *testing.T) {
	remote := dt.Remote(t, "demo", "solo")
	dt.Commit(t, remote, map[string]string{dt.ClaimPath: renameClaim})
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	if _, err := g.AddTenantCatalogueSource(ctx, "demo", "ours", "https://demo.example.com/apps", gitops.ByCluster, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddTenantCatalogueSource(ctx, "demo", "theirs", "https://demo.example.com/more", gitops.ByCluster, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")
	for _, c := range []struct {
		tenant, old, new string
		want             error
	}{
		{"", "nowhere", "acme", gitops.ErrCatalogueNotFound},
		{"", "ours", "acme", gitops.ErrCatalogueNotFound}, // a tenant's, not the cluster's
		{"", "partner", "main", gitops.ErrCatalogueNameTaken},
		{"", "partner", "ours", gitops.ErrCatalogueNameTaken}, // demo has it
		{"", "partner", "Acme", gitops.ErrInvalidName},
		{"", "part ner", "acme", gitops.ErrInvalidName},
		{"demo", "nowhere", "acme", gitops.ErrCatalogueNotFound},
		{"demo", "main", "acme", gitops.ErrCatalogueNotFound}, // the cluster's, not demo's
		{"demo", "ours", "theirs", gitops.ErrCatalogueNameTaken},
		{"demo", "ours", "main", gitops.ErrCatalogueNameTaken}, // the cluster's, which demo sees
		{"solo", "ours", "acme", gitops.ErrCatalogueNotFound},  // demo's, not solo's
		{"demo", "ours", "-acme", gitops.ErrInvalidName},
		{"de mo", "ours", "acme", gitops.ErrInvalidName},
		{"nobody", "ours", "acme", gitops.ErrTenantNotFound},
	} {
		if _, err := g.RenameCatalogueSource(ctx, c.tenant, c.old, c.new, meta("u-ada")); !errors.Is(err, c.want) {
			t.Errorf("rename %q %s -> %s: %v, want %v", c.tenant, c.old, c.new, err, c.want)
		}
	}
	for _, tenant := range []string{"", "demo"} {
		old := "partner"
		if tenant != "" {
			old = "ours"
		}
		if res, err := g.RenameCatalogueSource(ctx, tenant, old, old, meta("u-ada")); err != nil || res.Status != "unchanged" || res.Changed {
			t.Errorf("rename %q %s to itself = %+v, %v", tenant, old, res, err)
		}
	}
	if dt.Git(t, "", "--git-dir", remote, "rev-parse", "main") != tip {
		t.Fatal("a refused rename committed something")
	}
}
