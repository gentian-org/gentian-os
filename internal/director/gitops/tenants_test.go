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
	res, err := g.DeclareTenant(ctx, "acme", spec, "export nightly of 2026-10-01", tenantMeta())
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
	if _, err := g.DeclareTenant(ctx, "acme", spec, "again", tenantMeta()); !errors.Is(err, gitops.ErrTenantExists) {
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

// On a single-tenant cluster the director refuses a new tenant, and an
// import, before anything reaches git: the operator would refuse it as well,
// but only once the commit had landed.
func TestASingleTenantClusterTakesNoNewTenant(t *testing.T) {
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
	if _, err := g.CreateTenant(context.Background(), gitops.NewTenant{Name: "acme"}, tenantMeta()); !errors.Is(err, gitops.ErrSingleTenancy) {
		t.Fatalf("create: err = %v", err)
	}
	if _, err := g.DeclareTenant(context.Background(), "acme", &gentianov1alpha1.TenantSpec{DisplayName: "Acme"}, "test", tenantMeta()); !errors.Is(err, gitops.ErrSingleTenancy) {
		t.Fatalf("import: err = %v", err)
	}
	if after := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); after != before {
		t.Fatal("a refused tenant still reached git")
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

// A cluster carrying tenants beside the platform is not switched to single
// tenancy: every other tenant would be refused and stranded. Retired first,
// it may be.
func TestACarryingClusterIsNotMadeSingleTenant(t *testing.T) {
	remote := dt.Remote(t, "platform", "acme")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	if _, err := g.SetClusterSettings(ctx, map[string]string{"tenancyMode": "single"}, tenantMeta()); !errors.Is(err, gitops.ErrSingleTenancy) || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("err = %v", err)
	}
	if _, err := g.RetireTenant(ctx, "acme", tenantMeta()); err != nil {
		t.Fatal(err)
	}
	if res, err := g.SetClusterSettings(ctx, map[string]string{"tenancyMode": "single"}, tenantMeta()); err != nil || !res.Changed {
		t.Fatalf("after retiring: %+v %v", res, err)
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

// The install writes one tenant itself: the first, before there is a
// director to ask (scripts/lib/bootstrap.sh, scaffold_first_tenant). What it
// writes is the manifest CreateTenant writes, plus the annotation that admits
// the tenant before the handover. Two templates in two languages would drift
// apart silently, so the shell's output is produced here and compared with
// the director's as data.
func TestTheInstallsFirstTenantIsTheManifestTheDirectorWrites(t *testing.T) {
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
		`set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; scaffold_first_tenant `+dt.Cluster)
	cmd.Dir = root
	cmd.Env = []string{
		"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "SCRIPT_DIR=" + root,
		"GENTIAN_DEPLOYMENTS_PATH=" + checkout, "GENTIAN_DEPLOYMENTS_CLUSTER_ID=" + dt.Cluster,
		"GENTIAN_FIRST_TENANT=acme", "GENTIAN_FIRST_TENANT_DISPLAY_NAME=Acme Ltd",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the install's scaffold: %v\n%s", err, out)
	}
	installed, err := os.ReadFile(filepath.Join(checkout, dt.TenantPath("acme")))
	if err != nil {
		t.Fatal(err)
	}

	// The director's.
	remote := dt.Remote(t, "demo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	if _, err := g.CreateTenant(context.Background(), gitops.NewTenant{Name: "acme", DisplayName: "Acme Ltd"}, tenantMeta()); err != nil {
		t.Fatal(err)
	}

	var theirs, ours map[string]any
	if err := yaml.Unmarshal(installed, &theirs); err != nil {
		t.Fatalf("the install's manifest does not parse: %v\n%s", err, installed)
	}
	if err := yaml.Unmarshal([]byte(dt.RemoteFile(t, remote, dt.TenantPath("acme"))), &ours); err != nil {
		t.Fatal(err)
	}
	annotations, _ := theirs["metadata"].(map[string]any)["annotations"].(map[string]any)
	const override = "gentianos.io/handover-override"
	if reason, _ := annotations[override].(string); reason == "" {
		t.Fatalf("the install's first tenant carries no reason under %s, and the tenant webhook admits it on nothing else", override)
	}
	delete(annotations, override)
	if !reflect.DeepEqual(theirs, ours) {
		t.Fatalf("the install's first tenant is not the manifest the director writes.\ninstall:\n%s\ndirector:\n%s",
			installed, dt.RemoteFile(t, remote, dt.TenantPath("acme")))
	}
	for _, name := range []string{"kustomization.yaml"} {
		theirs, err := os.ReadFile(filepath.Join(checkout, filepath.Dir(dt.TenantPath("acme")), name))
		if err != nil {
			t.Fatal(err)
		}
		if ours := dt.RemoteFile(t, remote, filepath.Dir(dt.TenantPath("acme"))+"/"+name); strings.TrimSpace(string(theirs)) != ours {
			t.Fatalf("%s differs.\ninstall:\n%s\ndirector:\n%s", name, theirs, ours)
		}
	}
}
