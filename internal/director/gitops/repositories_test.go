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
	"path/filepath"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// seedClaim commits one file into the cluster's claims, the way the
// installer's scaffold does.
func seedClaim(t *testing.T, remote, name, body string) {
	t.Helper()
	seed := dt.Clone(t, remote)
	path := filepath.Join(seed, "clusters", dt.Cluster, "kernel", "claims", name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dt.Git(t, seed, "add", "-A")
	dt.Git(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-m", "seed "+name)
	dt.Git(t, seed, "push", "origin", "HEAD:main")
}

const installerDeployments = `# The repository this cluster is described by.
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: deployments
  namespace: kernel-provisioning
spec:
  type: git
  role: deployments
  writable: true
  branch: main
  endpoints:
    inCluster: https://git.example.com/old/deployments
    external: https://git.public.example.com/old/deployments
  credential:
    vaultPath: gentian-os/kernel/repositories/deployments
    displayName: "Deployments repository write access"
    phase: bootstrap
    authType: bearer
    validate:
      type: git-https
`

func repoMeta() gitops.Meta {
	return gitops.Meta{Author: gitops.Person{Name: "Alice", Email: "alice@example.com"}, Subject: "u-alice",
		RequestID: "req-repo-1", Decision: "can_write_credential cluster:" + dt.Cluster}
}

// A repository the installer declared is changed where it is declared, and a
// change of address keeps everything this API cannot express. The deployments
// repository's credential is read as a bearer token from a path the installer
// chose; losing that to a change of address would cost the cluster its push
// credential.
func TestChangingADeclaredRepositoryKeepsWhatWasNotAsked(t *testing.T) {
	remote := dt.Remote(t, "demo")
	seedClaim(t, remote, "deployments-repository.yaml", installerDeployments)
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()

	change := gitops.RepositoryDeclaration{Role: "deployments", Type: "git", URL: "https://git.example.com/new/deployments", Branch: "main", Writable: true}
	_, err := g.DeclareRepository(ctx, "", "deployments", change, repoMeta())
	var confirm *gitops.ConfirmationRequired
	if !errors.As(err, &confirm) || confirm.Name != "deployments" {
		t.Fatalf("repointing the deployments repository was not held for confirmation: %v", err)
	}

	change.Confirm = "deployments"
	res, err := g.DeclareRepository(ctx, "", "deployments", change, repoMeta())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || !res.Changed {
		t.Fatalf("result = %+v", res)
	}
	files := dt.Git(t, "", "--git-dir", remote, "ls-tree", "-r", "--name-only", "main")
	if strings.Count(files, "repository.yaml") != 1 {
		t.Fatalf("the repository is now declared twice:\n%s", files)
	}
	file := dt.RemoteFile(t, remote, "clusters/"+dt.Cluster+"/kernel/claims/deployments-repository.yaml")
	for _, want := range []string{
		"inCluster: https://git.example.com/new/deployments",
		"external: https://git.public.example.com/old/deployments",
		"namespace: kernel-provisioning",
		"vaultPath: gentian-os/kernel/repositories/deployments",
		"phase: bootstrap", "authType: bearer", "type: git-https",
		"writable: true", "role: deployments",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("the declaration lost %q:\n%s", want, file)
		}
	}
	if strings.Contains(file, "inCluster: https://git.example.com/old") {
		t.Fatalf("the address did not move:\n%s", file)
	}
}

// A tenant cannot take a name the cluster declared, whatever file it is in,
// and a repository that shares its file with other objects is not rewritten.
func TestANameIsTakenWhereverItIsDeclared(t *testing.T) {
	remote := dt.Remote(t, "demo")
	seedClaim(t, remote, "deployments-repository.yaml", installerDeployments)
	seedClaim(t, remote, "sources.yaml", `apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: charts
spec:
  type: oci
  role: apps
  endpoints:
    inCluster: oci://registry.example.com/charts
---
apiVersion: gentianos.io/v1alpha1
kind: Repository
metadata:
  name: more-charts
spec:
  type: oci
  role: apps
  endpoints:
    inCluster: oci://registry.example.com/more
`)
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	before := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	for _, name := range []string{"deployments", "charts", "more-charts"} {
		d := gitops.RepositoryDeclaration{Role: "apps", Type: "git", URL: "https://evil.example.com/x.git", Confirm: name}
		if _, err := g.DeclareRepository(ctx, "demo", name, d, repoMeta()); !errors.Is(err, gitops.ErrRepositoryNotFound) {
			t.Errorf("tenant demo declaring %s: %v", name, err)
		}
		if _, err := g.RemoveRepository(ctx, "demo", name, name, repoMeta()); !errors.Is(err, gitops.ErrRepositoryNotFound) {
			t.Errorf("tenant demo removing %s: %v", name, err)
		}
	}
	// The cluster's own, in a file with a second object: refused, not
	// rewritten around its neighbour.
	d := gitops.RepositoryDeclaration{Role: "apps", Type: "oci", URL: "oci://registry.example.com/charts"}
	if _, err := g.DeclareRepository(ctx, "", "charts", d, repoMeta()); !errors.Is(err, gitops.ErrRepositoryNotRewritable) {
		t.Errorf("a repository sharing its file: %v", err)
	}
	if _, err := g.RemoveRepository(ctx, "", "charts", "charts", repoMeta()); !errors.Is(err, gitops.ErrRepositoryNotRewritable) {
		t.Errorf("removing a repository sharing its file: %v", err)
	}
	if after := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main"); after != before {
		t.Fatal("a refused request moved the repository")
	}
	// A tenant nobody declared has nowhere to put one.
	if _, err := g.DeclareRepository(ctx, "ghost", "x", gitops.RepositoryDeclaration{Role: "apps", Type: "git", URL: "https://x.example/x.git"}, repoMeta()); !errors.Is(err, gitops.ErrTenantNotFound) {
		t.Errorf("a repository for a tenant that does not exist: %v", err)
	}
}
