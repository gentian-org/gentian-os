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
	"os"
	"path/filepath"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// claimWith rewrites the cluster claim on the remote and returns a GitOps
// reading it.
func claimWith(t *testing.T, spec string) *gitops.GitOps {
	t.Helper()
	remote := dt.Remote(t, "demo")
	edit := dt.Clone(t, remote)
	path := filepath.Join(edit, "clusters", dt.Cluster, "kernel", "claims", "cluster.yaml")
	body := "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: " + dt.Cluster +
		"\nspec:\n  kernelDomain: " + dt.KernelDomain + "\n" + spec
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dt.Git(t, edit, "add", "-A")
	dt.Git(t, edit, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "--allow-empty", "-m", "claim")
	dt.Git(t, edit, "push", "origin", "HEAD:main")
	return gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
}

// The list is the claim's, which is the whole point of AD-14: naming a
// catalogue source is a commit somebody can review, not a Deployment's
// environment.
func TestCatalogueSourcesComeFromTheClusterClaim(t *testing.T) {
	g := claimWith(t, `  catalogue:
    sources:
      - name: main
        url: https://store.example.com/catalogue
      - name: in-house
        url: https://git.example.com/profiles
`)
	got, err := g.CatalogueSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("sources = %+v", got)
	}
	if got[0].Name != "main" || got[0].URL != "https://store.example.com/catalogue" {
		t.Fatalf("first source = %+v", got[0])
	}
	if got[1].Name != "in-house" || got[1].URL != "https://git.example.com/profiles" {
		t.Fatalf("second source = %+v", got[1])
	}
}

// One malformed entry must not stop a director from serving: it costs installs
// from that catalogue, which is where somebody will see it, and not the
// console.
func TestAMalformedSourceIsSkippedRatherThanFatal(t *testing.T) {
	g := claimWith(t, `  catalogue:
    sources:
      - name: cleartext
        url: http://store.example.com/catalogue
      - name: ""
        url: https://store.example.com/catalogue
      - name: good
        url: https://store.example.com/catalogue
`)
	got, err := g.CatalogueSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("sources = %+v", got)
	}
}

// A cluster that declares none materialises nothing, which is a cluster whose
// profiles arrive some other way -- not an error.
func TestAClaimWithNoCatalogueYieldsNoSources(t *testing.T) {
	g := claimWith(t, "")
	got, err := g.CatalogueSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("sources = %+v", got)
	}
}
