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

package gitops_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	if acme.DisplayName != "Acme Ltd" || acme.Realm != "acme" {
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
