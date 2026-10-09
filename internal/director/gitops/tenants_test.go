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
	"os/exec"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

func tenantMeta() gitops.Meta {
	return gitops.Meta{
		Author:    gitops.Person{Name: "Ada Lovelace", Email: "ada@example.com"},
		Subject:   "u-ada",
		RequestID: "req-tenant-1",
		Decision:  "can_configure cluster:demo-cluster",
	}
}

// Bringing a customer on is one commit, and what it commits is a manifest a
// person could have written by hand.
func TestCreatingATenantIsOneReviewableCommit(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	res, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme", DisplayName: "Acme Ltd"}, tenantMeta())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "created" || !res.Changed {
		t.Fatalf("result = %+v", res)
	}
	if tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); tip != res.Commit {
		t.Fatalf("result names %s, remote is at %s", res.Commit, tip)
	}

	// The file is in the remote, under this cluster's tenants tree.
	path := "clusters/" + dt.Cluster + "/tenants/acme/tenant.yaml"
	manifest := dt.Git(t, "", "--git-dir", remote, "show", "main:"+path)
	for _, want := range []string{"kind: Tenant", "name: acme", "displayName: Acme Ltd", "keycloakRealm: acme"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest is missing %q:\n%s", want, manifest)
		}
	}
	// Retain, not Delete. Retiring a tenant must not be the same gesture as
	// destroying its data, and the default has to be the safe one.
	if !strings.Contains(manifest, "deletionPolicy: Retain") {
		t.Error("a new tenant must default to Retain")
	}
	// The comments are the point of writing this as text. A reviewer reading
	// the commit should not have to look anything up.
	if !strings.Contains(manifest, "# Tenant acme, brought on through the director.") {
		t.Error("the manifest lost its explanation")
	}
}

func TestATenantIsListedWithWhatTheManifestSays(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme", DisplayName: "Acme Ltd"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	tenants, err := g.TenantDetails(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var acme *gitops.Tenant
	for i := range tenants {
		if tenants[i].Name == "acme" {
			acme = &tenants[i]
		}
	}
	if acme == nil {
		t.Fatalf("acme is not listed: %+v", tenants)
	}
	if acme.DisplayName != "Acme Ltd" || acme.Realm != "acme" || acme.LoginDomain != "acme."+dt.KernelDomain {
		t.Fatalf("listing = %+v", *acme)
	}
	if acme.Protected {
		t.Error("an ordinary tenant is not protected")
	}
}

// A name that cannot be a namespace, a realm and a hostname is refused before
// anything is written, because none of those can be renamed later.
func TestATenantNameThatCannotBecomeAHostnameIsRefused(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	for _, bad := range []string{"", "Acme", "acme.ltd", "-acme", "acme_ltd", "../escape"} {
		if _, err := g.CreateTenant(context.Background(), gitops.NewTenant{Name: bad}, tenantMeta()); !errors.Is(err, gitops.ErrInvalidName) {
			t.Errorf("CreateTenant(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
}

func TestCreatingATenantTwiceIsARefusalNotASecondManifest(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme", DisplayName: "Other"}, tenantMeta()); !errors.Is(err, gitops.ErrTenantExists) {
		t.Fatalf("second create = %v, want ErrTenantExists", err)
	}
}

// Retiring stops git describing the tenant, which is what removes it: Argo CD
// prunes what git no longer names.
func TestRetiringATenantRemovesItsDirectory(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	res, err := g.RetireTenant(ctx, "acme", tenantMeta())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "retired" || !res.Changed {
		t.Fatalf("result = %+v", res)
	}
	tree := dt.Git(t, "", "--git-dir", remote, "ls-tree", "-r", "--name-only", "main")
	if strings.Contains(tree, "tenants/acme/") {
		t.Fatalf("acme is still described in git:\n%s", tree)
	}
	tenants, err := g.TenantDetails(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tn := range tenants {
		if tn.Name == "acme" {
			t.Fatal("acme is still listed after being retired")
		}
	}
}

// The platform tenant carries the kernel realm every administrator signs in
// against. Retiring it is not a tenant going away, it is the cluster locking
// everyone out, so the director refuses however the request was authorised.
func TestThePlatformTenantCannotBeRetiredThroughTheDirector(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if _, err := g.RetireTenant(context.Background(), "platform", tenantMeta()); !errors.Is(err, gitops.ErrTenantProtected) {
		t.Fatalf("retire platform = %v, want ErrTenantProtected", err)
	}
}

func TestRetiringSomethingThatIsNotThereSaysSo(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if _, err := g.RetireTenant(context.Background(), "nobody", tenantMeta()); !errors.Is(err, gitops.ErrTenantNotFound) {
		t.Fatalf("retire nobody = %v, want ErrTenantNotFound", err)
	}
}

// A purge is asked for in one commit and finished in another. The first says
// Delete and marks the manifest, keeping every comment; asking again changes
// nothing; and the mark is what finds it again after a restart.
func TestAPurgeIsRequestedBeforeTheTenantIsRemoved(t *testing.T) {
	ctx := context.Background()
	remote := dt.Remote(t)
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme", DisplayName: "Acme Ltd"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	res, err := g.RequestTenantPurge(ctx, "acme", now, gitops.PurgeOptions{KeepBundles: true}, tenantMeta())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "purge_requested" || !res.Changed {
		t.Fatalf("result = %+v, want a purge_requested commit", res)
	}

	text := dt.RemoteFile(t, remote, dt.TenantPath("acme"))
	var doc struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			DeletionPolicy string `json:"deletionPolicy"`
			DisplayName    string `json:"displayName"`
			Deletion       struct {
				KeepBundles bool `json:"keepBundles"`
			} `json:"deletion"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("the manifest no longer parses: %v\n%s", err, text)
	}
	if doc.Spec.DeletionPolicy != "Delete" {
		t.Fatalf("deletionPolicy = %q, want Delete", doc.Spec.DeletionPolicy)
	}
	if !doc.Spec.Deletion.KeepBundles {
		t.Fatalf("deletion.keepBundles not set:\n%s", text)
	}
	if doc.Metadata.Annotations[gitops.PurgeAnnotation] != "2026-10-02T12:00:00Z" {
		t.Fatalf("annotations = %v, want the purge mark", doc.Metadata.Annotations)
	}
	if doc.Metadata.Annotations["argocd.argoproj.io/sync-wave"] != "2" || doc.Spec.DisplayName != "Acme Ltd" {
		t.Fatalf("the edit disturbed the rest of the manifest:\n%s", text)
	}
	if !strings.Contains(text, "# Tenant acme, brought on through the director.") {
		t.Fatal("the manifest's comments were lost")
	}

	again, err := g.RequestTenantPurge(ctx, "acme", now.Add(time.Hour), gitops.PurgeOptions{KeepBundles: true}, tenantMeta())
	if err != nil || again.Changed || again.Status != "purge_pending" {
		t.Fatalf("second request = %+v, %v; want purge_pending and no commit", again, err)
	}
	pending, err := g.PendingPurges(ctx)
	if err != nil || len(pending) != 1 || pending[0] != "acme" {
		t.Fatalf("pending = %v, %v; want [acme]", pending, err)
	}
}

func TestThePlatformTenantCannotBePurged(t *testing.T) {
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if _, err := g.RequestTenantPurge(context.Background(), "platform", time.Now(), gitops.PurgeOptions{}, tenantMeta()); !errors.Is(err, gitops.ErrTenantProtected) {
		t.Fatalf("purge platform = %v, want ErrTenantProtected", err)
	}
}

// An import declares the tenant from the bundle's spec, as one commit, with
// the apps the bundle recorded -- and never with the bundle's deletion
// policy, so a tenant exported mid-purge does not arrive with its purge.
func TestAnImportDeclaresTheTenantFromTheBundlesSpec(t *testing.T) {
	ctx := context.Background()
	remote := dt.Remote(t)
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	spec := &gentianov1alpha1.TenantSpec{
		DisplayName:    "Acme Ltd",
		DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
		Deletion:       &gentianov1alpha1.TenantDeletion{KeepBundles: true},
		Apps:           []gentianov1alpha1.TenantApp{{Profile: "nextcloud-base-ce"}},
		// Nor with its catalogues, or with leave to add its own.
		Catalogue: &gentianov1alpha1.TenantCatalogue{Delegated: true, Sources: []gentianov1alpha1.TenantCatalogueSource{
			{Name: "theirs", URL: "https://elsewhere.example.com/apps", AddedBy: "tenant"},
		}},
	}
	res, err := g.DeclareTenant(ctx, gitops.ImportedTenant{Name: "acme", Spec: spec, Origin: "export nightly of 2026-10-01"}, tenantMeta())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "imported" || !res.Changed {
		t.Fatalf("result = %+v", res)
	}
	text := dt.RemoteFile(t, remote, dt.TenantPath("acme"))
	var doc struct {
		Kind string `json:"kind"`
		Spec struct {
			DisplayName    string `json:"displayName"`
			DeletionPolicy string `json:"deletionPolicy"`
			Deletion       *struct {
				KeepBundles bool `json:"keepBundles"`
			} `json:"deletion"`
			Apps []struct {
				Profile string `json:"profile"`
			} `json:"apps"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("manifest does not parse: %v\n%s", err, text)
	}
	if doc.Kind != "Tenant" || doc.Spec.DisplayName != "Acme Ltd" || len(doc.Spec.Apps) != 1 || doc.Spec.Apps[0].Profile != "nextcloud-base-ce" {
		t.Fatalf("manifest does not carry the bundle's spec:\n%s", text)
	}
	if doc.Spec.DeletionPolicy != "Retain" || doc.Spec.Deletion != nil {
		t.Fatalf("the bundle's purge travelled with it:\n%s", text)
	}
	if strings.Contains(text, "catalogue") || strings.Contains(text, "delegated") {
		t.Fatalf("the bundle's catalogues travelled with it:\n%s", text)
	}
	if !strings.Contains(text, "imported from a bundle") {
		t.Fatal("the manifest does not say where it came from")
	}
	if _, err := g.DeclareTenant(ctx, gitops.ImportedTenant{Name: "acme", Spec: spec, Origin: "again"}, tenantMeta()); !errors.Is(err, gitops.ErrTenantExists) {
		t.Fatalf("second import = %v, want ErrTenantExists", err)
	}
	tenants, err := g.TenantDetails(ctx)
	if err != nil || len(tenants) != 1 || tenants[0].Apps[0] != "nextcloud-base-ce" {
		t.Fatalf("tenants = %+v, %v", tenants, err)
	}
}

// A tenant's login domain follows the TenantDomain an extension commits
// beside its manifest, and only that: no file means <tenant>.<kernel>, and
// one naming a host on the kernel domain is ignored, as the operator ignores
// it.
func TestATenantsLoginDomainFollowsItsTenantDomain(t *testing.T) {
	remote := dt.Remote(t, "acme")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if got, err := g.TenantLoginDomain(ctx, "acme"); err != nil || got != "acme."+dt.KernelDomain {
		t.Fatalf("without a TenantDomain: %q, %v", got, err)
	}

	bind := func(domain string) {
		t.Helper()
		seed := dt.Clone(t, remote)
		path := filepath.Join(seed, "clusters", dt.Cluster, "tenants", "acme", gitops.TenantDomainFile)
		doc := "apiVersion: gentianos.io/v1alpha1\nkind: TenantDomain\nmetadata:\n  name: acme\nspec:\n  domain: " + domain + "\n"
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		dt.Git(t, seed, "add", "-A")
		dt.Git(t, seed, "-c", "user.name=ext", "-c", "user.email=ext@example.com", "commit", "-m", "bind "+domain)
		dt.Git(t, seed, "push", "origin", "HEAD:main")
		g = gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	}

	bind("acme.example")
	if got, err := g.TenantLoginDomain(ctx, "acme"); err != nil || got != "acme.example" {
		t.Fatalf("bound: %q, %v", got, err)
	}
	bind("x.acme." + dt.KernelDomain)
	if got, err := g.TenantLoginDomain(ctx, "acme"); err != nil || got != "acme."+dt.KernelDomain {
		t.Fatalf("on the kernel domain: %q, %v", got, err)
	}
}

// On a single-tenancy cluster the director refuses a second user tenant, and
// an import of one, before anything reaches git: the operator would refuse it
// as well, but only once the commit had landed. The one user tenant, named
// user, is admitted; the platform tenant is not counted.
func TestASingleTenancyClusterTakesExactlyOneUserTenant(t *testing.T) {
	remote := dt.Remote(t, "platform")
	seed := dt.Clone(t, remote)
	claim := filepath.Join(seed, "clusters", dt.Cluster, "kernel", "claims", "cluster.yaml")
	b, err := os.ReadFile(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claim, append(b, []byte("  tenancyMode: single\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	dt.Git(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-am", "single")
	dt.Git(t, seed, "push", "origin", "HEAD:main")
	before := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	_, err = g.CreateTenant(context.Background(), gitops.NewTenant{Name: "acme"}, tenantMeta())
	if !errors.Is(err, gitops.ErrSingleTenancy) {
		t.Fatalf("create: err = %v", err)
	}
	for _, want := range []string{"tenancy mode is single", `"user"`, `"acme"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if _, err := g.DeclareTenant(context.Background(), gitops.ImportedTenant{Name: "acme", Spec: &gentianov1alpha1.TenantSpec{DisplayName: "Acme"}, Origin: "test"}, tenantMeta()); !errors.Is(err, gitops.ErrSingleTenancy) {
		t.Fatalf("import: err = %v", err)
	}
	if after := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); after != before {
		t.Fatal("a refused tenant still reached git")
	}

	// The one user tenant: admitted, with the platform tenant already there.
	if res, err := g.CreateTenant(context.Background(), gitops.NewTenant{Name: "user"}, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("the one user tenant was refused: %+v %v", res, err)
	}
	// Its people sign in under the cluster's own domain; the platform's too,
	// in another realm.
	kernel, err := g.KernelDomain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d, err := g.TenantLoginDomain(context.Background(), "user"); err != nil || d != kernel {
		t.Fatalf("user tenant login domain = %q, %v; want %q", d, err, kernel)
	}
	// A second is still refused, and so is a second of the same name.
	if _, err := g.CreateTenant(context.Background(), gitops.NewTenant{Name: "beta"}, tenantMeta()); !errors.Is(err, gitops.ErrSingleTenancy) {
		t.Fatalf("second user tenant: err = %v", err)
	}
	if _, err := g.CreateTenant(context.Background(), gitops.NewTenant{Name: "user"}, tenantMeta()); !errors.Is(err, gitops.ErrTenantExists) {
		t.Fatalf("user again: err = %v", err)
	}
}

// Under multi a tenant named user is an ordinary tenant, beside any number
// of others, on a domain of its own.
func TestUnderMultiATenantNamedUserIsOrdinary(t *testing.T) {
	remote := dt.Remote(t, "platform", "acme")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	for _, name := range []string{"user", "beta"} {
		if res, err := g.CreateTenant(ctx, gitops.NewTenant{Name: name}, tenantMeta()); err != nil || !res.Changed {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
	}
	kernel, err := g.KernelDomain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := g.TenantLoginDomain(ctx, "user"); err != nil || d != "user."+kernel {
		t.Fatalf("login domain = %q, %v", d, err)
	}
}

// A custom domain is one commit of the TenantDomain and its kustomization
// entry, and removing it is one more. A name that is not a hostname, one on
// the kernel domain and one another tenant holds are refused before git.
func TestACustomDomainIsOneCommitAndRefusedWhereItCannotBe(t *testing.T) {
	remote := dt.Remote(t, "acme", "beta")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	dir := "clusters/" + dt.Cluster + "/tenants/acme/"

	res, err := g.SetTenantDomain(ctx, "acme", " Acme.Example ", tenantMeta())
	if err != nil || !res.Changed {
		t.Fatalf("bind: %+v, %v", res, err)
	}
	if doc := dt.RemoteFile(t, remote, dir+gitops.TenantDomainFile); !strings.Contains(doc, "kind: TenantDomain") || !strings.Contains(doc, "domain: acme.example") {
		t.Fatalf("domain.yaml:\n%s", doc)
	}
	if k := dt.RemoteFile(t, remote, dir+"kustomization.yaml"); !strings.Contains(k, "- "+gitops.TenantDomainFile) {
		t.Fatalf("kustomization does not list it:\n%s", k)
	}
	if got, _ := g.TenantLoginDomain(ctx, "acme"); got != "acme.example" {
		t.Fatalf("login domain = %q", got)
	}

	before := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")
	for _, bad := range []struct{ tenant, domain string }{
		{"beta", "acme.example"},
		{"beta", "beta." + dt.KernelDomain},
		{"beta", "not a host"},
	} {
		if _, err := g.SetTenantDomain(ctx, bad.tenant, bad.domain, tenantMeta()); !errors.Is(err, gitops.ErrInvalidDomain) {
			t.Errorf("%s -> %q: err = %v", bad.tenant, bad.domain, err)
		}
	}
	if after := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); after != before {
		t.Fatal("a refused domain reached git")
	}

	if res, err := g.SetTenantDomain(ctx, "acme", "", tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("unbind: %+v, %v", res, err)
	}
	if k := dt.RemoteFile(t, remote, dir+"kustomization.yaml"); strings.Contains(k, gitops.TenantDomainFile) {
		t.Fatalf("kustomization still lists it:\n%s", k)
	}
	if got, _ := g.TenantLoginDomain(ctx, "acme"); got != "acme."+dt.KernelDomain {
		t.Fatalf("login domain after unbinding = %q", got)
	}
}

// No domain is bound on a single-tenancy cluster, to its user tenant or any
// other, and none to the platform tenant under either mode: each is refused
// before git, whatever the domain. Unbinding is refused to neither.
func TestADomainIsBoundToNoTenantOfASingleTenancyClusterAndNeverToThePlatformTenant(t *testing.T) {
	remote := dt.Remote(t, "platform", "user", "acme")
	dt.Commit(t, remote, map[string]string{
		dt.TenantPath("platform"): "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: platform\n" +
			"spec:\n  displayName: Platform\n  isolation:\n    mode: namespace\n    keycloakRealm: kernel\n",
	})
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	// Many tenants: only the platform tenant is refused.
	if _, err := g.SetTenantDomain(ctx, "platform", "acme.example", tenantMeta()); !errors.Is(err, gitops.ErrPlatformTenantDomain) {
		t.Fatalf("the platform tenant under multi: err = %v", err)
	}
	if res, err := g.SetTenantDomain(ctx, "acme", "acme.example", tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("a user tenant under multi: %+v, %v", res, err)
	}

	claim := dt.RemoteFile(t, remote, dt.ClaimPath)
	dt.Commit(t, remote, map[string]string{dt.ClaimPath: strings.TrimRight(claim, "\n") + "\n  tenancyMode: single\n"})
	g = gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	before := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")
	for _, tenant := range []string{"user", "acme"} {
		_, err := g.SetTenantDomain(ctx, tenant, "other.example", tenantMeta())
		if !errors.Is(err, gitops.ErrSingleTenancy) || !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("%s under single: err = %v", tenant, err)
		}
	}
	if _, err := g.SetTenantDomain(ctx, "platform", "other.example", tenantMeta()); !errors.Is(err, gitops.ErrPlatformTenantDomain) {
		t.Errorf("the platform tenant under single: err = %v", err)
	}
	if after := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); after != before {
		t.Fatal("a refused domain reached git")
	}
	// The tenant bound before the mode changed is put back.
	if res, err := g.SetTenantDomain(ctx, "acme", "", tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("unbind under single: %+v, %v", res, err)
	}
	if res, err := g.SetTenantDomain(ctx, "platform", "", tenantMeta()); err != nil || res.Changed {
		t.Fatalf("unbind of the platform tenant, which has none: %+v, %v", res, err)
	}
}

// The brand is one commit of a Branding among the cluster's declarations,
// read back as written; one the pages could not show is refused before git.
func TestTheBrandIsCommittedAndABrokenOneIsRefused(t *testing.T) {
	remote := dt.Remote(t)
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, found, err := g.ClusterBranding(ctx); err != nil || found {
		t.Fatalf("before: found=%v err=%v", found, err)
	}
	spec := gentianov1alpha1.BrandingSpec{
		Identity: gentianov1alpha1.BrandIdentity{Name: "Acme Cloud"},
		Tokens:   &runtime.RawExtension{Raw: []byte(`{"color":{"$type":"color","brand":{"500":{"$value":"#c0392b"}}}}`)},
	}
	if res, err := g.SetClusterBranding(ctx, spec, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("set: %+v %v", res, err)
	}
	doc := dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/kernel/claims/"+gitops.BrandingFile)
	for _, want := range []string{"kind: Branding", "name: default", "name: Acme Cloud", "#c0392b"} {
		if !strings.Contains(doc, want) {
			t.Errorf("branding.yaml lacks %q:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "status") || strings.Contains(doc, "creationTimestamp") {
		t.Errorf("branding.yaml carries cluster state:\n%s", doc)
	}
	got, found, err := g.ClusterBranding(ctx)
	if err != nil || !found || got.Identity.Name != "Acme Cloud" {
		t.Fatalf("read back: %+v %v %v", got, found, err)
	}

	broken := spec
	broken.Tokens = &runtime.RawExtension{Raw: []byte(`{"color":{"$type":"color","brand":{"500":{"$value":"red; }"}}}}`)}
	if _, err := g.SetClusterBranding(ctx, broken, tenantMeta()); !errors.Is(err, gitops.ErrInvalidBranding) {
		t.Fatalf("broken: err = %v", err)
	}
}

// multi -> single on a cluster carrying more than a single-tenancy cluster
// may is refused, loudly and with nothing written: every tenant but the
// platform's and one named user would be refused by the operator and left
// standing. Retired first, the switch goes through; a tenant named user may
// stay, and the platform tenant is not counted. single -> multi is always
// fine.
func TestACarryingClusterIsNotMadeSingleTenancy(t *testing.T) {
	remote := dt.Remote(t, "platform", "acme", "user")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	before := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	_, err := g.SetClusterSettings(ctx, map[string]string{"tenancyMode": "single"}, tenantMeta())
	if !errors.Is(err, gitops.ErrSingleRefused) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"acme", `"user"`, "Nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "carries acme, user") || strings.Contains(err.Error(), "platform,") {
		t.Errorf("the refusal names a tenant a single-tenancy cluster may carry: %v", err)
	}
	if after := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); after != before {
		t.Fatal("a refused mode change still reached git")
	}
	if _, err := g.RetireTenant(ctx, "acme", tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if res, err := g.SetClusterSettings(ctx, map[string]string{"tenancyMode": "single"}, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("after retiring: %+v %v", res, err)
	}
	if res, err := g.SetClusterSettings(ctx, map[string]string{"tenancyMode": "multi"}, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("single -> multi: %+v %v", res, err)
	}
}

// The platform tenant takes no apps and no add-ons, by whichever route the
// write arrives: the manifest about to be edited is what refuses, so a caller
// that did not ask first is refused all the same, and nothing is committed.
// A tenant is the platform's by the realm it adopts, not by its name.
func TestThePlatformTenantsManifestRefusesAppsAndAddons(t *testing.T) {
	remote := dt.Remote(t, "demo", "ops")
	seed := dt.Clone(t, remote)
	manifest := "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: ops\nspec:\n  displayName: Platform\n" +
		"  isolation:\n    mode: namespace\n    keycloakRealm: kernel\n  apps:\n  - profile: nextcloud\n"
	if err := os.WriteFile(filepath.Join(seed, dt.TenantPath("ops")), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	dt.Git(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-am", "ops adopts the kernel realm")
	dt.Git(t, seed, "push", "origin", "HEAD:main")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	before := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	if platform, err := g.IsPlatformTenant(ctx, "ops"); err != nil || !platform {
		t.Fatalf("a tenant adopting the kernel realm: platform=%v err=%v", platform, err)
	}
	if platform, err := g.IsPlatformTenant(ctx, "demo"); err != nil || platform {
		t.Fatalf("a tenant naming no realm: platform=%v err=%v", platform, err)
	}
	if _, err := g.IsPlatformTenant(ctx, "nobody"); !errors.Is(err, gitops.ErrTenantNotFound) {
		t.Fatalf("a tenant that does not exist: %v", err)
	}
	if _, err := g.Install(ctx, "ops", "wiki", "", tenantMeta()); !errors.Is(err, gitops.ErrPlatformTenant) {
		t.Fatalf("an install in the platform tenant: %v", err)
	}
	if _, err := g.SetAddons(ctx, "ops", "nextcloud", []string{"calendar"}, tenantMeta()); !errors.Is(err, gitops.ErrPlatformTenant) {
		t.Fatalf("addons in the platform tenant: %v", err)
	}
	if after := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); after != before {
		t.Fatal("a refused write committed something")
	}

	// A tenant the director creates names its own realm, and installs.
	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if platform, err := g.IsPlatformTenant(ctx, "acme"); err != nil || platform {
		t.Fatalf("a tenant naming its own realm: platform=%v err=%v", platform, err)
	}
	if res, err := g.Install(ctx, "acme", "wiki", "", tenantMeta()); err != nil || res.Status != "installed" {
		t.Fatalf("an install in a user tenant: %+v %v", res, err)
	}
}

// The install writes one tenant itself: the user tenant of a single-tenancy
// cluster (scripts/lib/bootstrap.sh, scaffold_user_tenant), committed with
// the cluster's definition before there is a director to ask. What it writes
// is the manifest CreateTenant writes, and nothing more -- in particular no
// annotation that would admit the tenant ahead of the handover. Two templates
// in two languages would drift apart silently, so the shell's output is
// produced here and compared with the director's as data.
func TestTheInstallsUserTenantIsTheManifestTheDirectorWrites(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash to run the install's scaffold with")
	}
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")

	// The install's, into a deployments checkout of its own.
	checkout := t.TempDir()
	dt.Git(t, checkout, "init", "--initial-branch=main")
	cmd := exec.Command(bash, "-c",
		`set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; scaffold_user_tenant `+dt.Cluster)
	cmd.Dir = root
	cmd.Env = []string{
		"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "SCRIPT_DIR=" + root,
		"GENTIAN_DEPLOYMENTS_PATH=" + checkout, "GENTIAN_DEPLOYMENTS_CLUSTER_ID=" + dt.Cluster,
		"TENANCY_MODE=single",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the install's scaffold: %v\n%s", err, out)
	}
	installed, err := os.ReadFile(filepath.Join(checkout, dt.TenantPath("user")))
	if err != nil {
		t.Fatal(err)
	}

	// The director's.
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if _, err := g.CreateTenant(context.Background(), gitops.NewTenant{Name: "user", DisplayName: "User"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}

	var theirs, ours map[string]any
	if err := yaml.Unmarshal(installed, &theirs); err != nil {
		t.Fatalf("the install's manifest does not parse: %v\n%s", err, installed)
	}
	if err := yaml.Unmarshal([]byte(dt.RemoteFile(t, remote, dt.TenantPath("user"))), &ours); err != nil {
		t.Fatal(err)
	}
	annotations, _ := theirs["metadata"].(map[string]any)["annotations"].(map[string]any)
	if _, overridden := annotations["gentianos.io/handover-override"]; overridden {
		t.Fatal("the install's user tenant asks to be admitted before the handover; it is created after it")
	}
	if !reflect.DeepEqual(theirs, ours) {
		t.Fatalf("the install's user tenant is not the manifest the director writes.\ninstall:\n%s\ndirector:\n%s",
			installed, dt.RemoteFile(t, remote, dt.TenantPath("user")))
	}
	for _, name := range []string{"kustomization.yaml"} {
		theirs, err := os.ReadFile(filepath.Join(checkout, filepath.Dir(dt.TenantPath("user")), name))
		if err != nil {
			t.Fatal(err)
		}
		if ours := dt.RemoteFile(t, remote, filepath.Dir(dt.TenantPath("user"))+"/"+name); strings.TrimSpace(string(theirs)) != ours {
			t.Fatalf("%s differs.\ninstall:\n%s\ndirector:\n%s", name, theirs, ours)
		}
	}
}

// A bundle's manifest states the names of the tenant it was taken of: its
// realm, its database prefix, its bucket prefix. An import copied them, so a
// tenant imported under another name beside the original was a second tenant
// on the original's realm, databases and buckets -- and the restore that
// followed replaced them. An imported tenant's names now follow from its own
// name, by the rule a created tenant's do, whatever the bundle states.
func TestATenantImportedUnderAnotherNameGetsItsOwnNames(t *testing.T) {
	ctx := context.Background()
	remote := dt.Remote(t)
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme", DisplayName: "Acme Ltd"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	// What an export of acme writes into its bundle: acme's own spec.
	var original struct {
		Spec gentianov1alpha1.TenantSpec `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(dt.RemoteFile(t, remote, dt.TenantPath("acme"))), &original); err != nil {
		t.Fatal(err)
	}
	if original.Spec.Isolation == nil || original.Spec.Isolation.KeycloakRealm != "acme" {
		t.Fatalf("the fixture does not state acme's names: %+v", original.Spec.Isolation)
	}
	original.Spec.Isolation.Namespace = "somewhere-else"

	pending := gitops.PendingImport{Source: "acme", Export: "nightly", Restore: "import-20261008-101500",
		Bundle: gentianov1alpha1.BundleRef{Bucket: "gentian-imports", Prefix: "20261008-101500-1a2b3c4d"}}
	if _, err := g.DeclareTenant(ctx, gitops.ImportedTenant{Name: "acme2", Spec: &original.Spec, Origin: "export nightly", Pending: pending}, tenantMeta()); err != nil {
		t.Fatal(err)
	}
	var imported struct {
		Spec gentianov1alpha1.TenantSpec `json:"spec"`
	}
	text := dt.RemoteFile(t, remote, dt.TenantPath("acme2"))
	if err := yaml.Unmarshal([]byte(text), &imported); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	iso := imported.Spec.Isolation
	if iso == nil || iso.KeycloakRealm != "acme2" || iso.DatabasePrefix != "acme2_" || iso.S3Prefix != "acme2-" || iso.Namespace != "" {
		t.Fatalf("the imported tenant's names are not its own: %+v\n%s", iso, text)
	}
	if iso.Mode != original.Spec.Isolation.Mode || imported.Spec.DisplayName != "Acme Ltd" {
		t.Errorf("the bundle's settings did not come: %+v", imported.Spec)
	}
	// Byte for byte: nothing of acme's names is anywhere in the manifest's spec.
	if strings.Contains(text, "keycloakRealm: acme\n") || strings.Contains(text, "databasePrefix: acme_\n") || strings.Contains(text, "s3Prefix: acme-\n") {
		t.Errorf("the original tenant's names are in the imported manifest:\n%s", text)
	}
	// And the original is as it was.
	if after := dt.RemoteFile(t, remote, dt.TenantPath("acme")); !strings.Contains(after, "keycloakRealm: acme\n") {
		t.Errorf("the original tenant's manifest changed:\n%s", after)
	}

	// The import is on record with the tenant, in the same commit, without a
	// key; and is not once it has finished.
	record := dt.RemoteFile(t, remote, strings.TrimSuffix(dt.TenantPath("acme2"), "tenant.yaml")+gitops.ImportFile)
	if !strings.Contains(record, `"restore": "import-20261008-101500"`) || !strings.Contains(record, `"source": "acme"`) {
		t.Fatalf("the record of the import:\n%s", record)
	}
	for _, secret := range []string{"passphrase", "identity", "decryption", "AGE-SECRET"} {
		if strings.Contains(record, secret) {
			t.Errorf("the record of an import names %q; a bundle's key is never written to git", secret)
		}
	}
	got, err := g.PendingImports(ctx)
	if err != nil || len(got) != 1 || got[0].Tenant != "acme2" || got[0].Bundle.Prefix != pending.Bundle.Prefix || got[0].Restore != pending.Restore {
		t.Fatalf("pending imports = %+v, %v", got, err)
	}
	if res, err := g.FinishImport(ctx, "acme2", tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("finish = %+v, %v", res, err)
	}
	if got, err := g.PendingImports(ctx); err != nil || len(got) != 0 {
		t.Fatalf("pending after it finished = %+v, %v", got, err)
	}
	if res, err := g.FinishImport(ctx, "acme2", tenantMeta()); err != nil || res.Changed {
		t.Fatalf("finishing twice = %+v, %v", res, err)
	}
	// The tenant's directory is still a kustomization of its manifest alone.
	if k := dt.RemoteFile(t, remote, strings.TrimSuffix(dt.TenantPath("acme2"), "tenant.yaml")+"kustomization.yaml"); strings.Contains(k, "import") {
		t.Errorf("the record is listed as a resource:\n%s", k)
	}
}

// Independent of how a tenant's names are arrived at: a tenant whose realm,
// database prefix or bucket prefix is another tenant's is not created and
// not imported. Nothing reaches git.
func TestATenantWhoseNamesAreAnothersIsRefused(t *testing.T) {
	ctx := context.Background()
	remote := dt.Remote(t)
	// A tenant that states names that are not its own name's: as the
	// platform tenant does, and as a hand-written manifest may.
	dt.Commit(t, remote, map[string]string{
		dt.TenantPath("legacy"): "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: legacy\nspec:\n  displayName: Legacy\n" +
			"  isolation:\n    keycloakRealm: acme\n    databasePrefix: shared_\n    s3Prefix: Globex-\n",
	})
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	before := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	// By the realm: creating "acme", whose realm would be legacy's.
	_, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "acme"}, tenantMeta())
	if !errors.Is(err, backup.ErrNamesTaken) || !strings.Contains(err.Error(), `the realm "acme" is tenant legacy's`) {
		t.Fatalf("create acme = %v", err)
	}
	// By the database prefix, and by the bucket prefix as it is used (lower
	// case): importing "shared" and "globex".
	_, err = g.DeclareTenant(ctx, gitops.ImportedTenant{Name: "shared", Spec: &gentianov1alpha1.TenantSpec{DisplayName: "S"}, Origin: "test"}, tenantMeta())
	if !errors.Is(err, backup.ErrNamesTaken) || !strings.Contains(err.Error(), `the database prefix "shared_" is tenant legacy's`) {
		t.Fatalf("import shared = %v", err)
	}
	_, err = g.DeclareTenant(ctx, gitops.ImportedTenant{Name: "globex", Spec: &gentianov1alpha1.TenantSpec{DisplayName: "G"}, Origin: "test"}, tenantMeta())
	if !errors.Is(err, backup.ErrNamesTaken) || !strings.Contains(err.Error(), `the bucket prefix "globex-" is tenant legacy's`) {
		t.Fatalf("import globex = %v", err)
	}
	if after := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); after != before {
		t.Fatal("a refused tenant still reached git")
	}
	// A tenant whose names are free is created beside it.
	if res, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "initech"}, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("create initech = %+v, %v", res, err)
	}

	// A manifest that cannot be read is not a tenant without names.
	dt.Commit(t, remote, map[string]string{dt.TenantPath("broken"): "spec: [this is: not a tenant\n"})
	if _, err := g.CreateTenant(ctx, gitops.NewTenant{Name: "umbrella"}, tenantMeta()); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("a tenant was created beside a manifest whose names cannot be read: %v", err)
	}
}
