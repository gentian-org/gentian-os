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
	"os/exec"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

const emptyKustomization = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"

func inRemote(remote, path string) bool {
	return exec.Command("git", "--git-dir", remote, "cat-file", "-e", "main:"+path).Run() == nil
}

// A profile is taken out of the catalogue directory whole -- its bundle, the
// carrier, both kustomization entries -- and the entries of the others stay
// as they were. Taking the last one out leaves the kustomization as it was
// before the first went in.
func TestRetiringAProfileRemovesItsFilesAndItsEntriesOnly(t *testing.T) {
	remote := dt.Remote(t, "demo")
	dt.Commit(t, remote, map[string]string{dt.CataloguePath("kustomization.yaml"): emptyKustomization})
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	for _, name := range []string{"shop", "notes"} {
		body := []byte(dt.ProfileYAML(name))
		if _, err := g.MaterialiseProfile(ctx, name, profilebundle.Digest(body), body, profilebundle.ClusterOrigin("main"), meta("u-ada")); err != nil {
			t.Fatal(err)
		}
	}

	res, err := g.RetireProfile(ctx, "shop", meta("u-ada"))
	if err != nil || res.Status != "removed" || !res.Changed || res.Commit == "" {
		t.Fatalf("retire = %+v, %v", res, err)
	}
	for _, file := range []string{"shop.yaml", gitops.BundleFile("shop")} {
		if inRemote(remote, dt.CataloguePath(file)) {
			t.Errorf("%s is still there", file)
		}
	}
	k := dt.RemoteFile(t, remote, dt.CataloguePath("kustomization.yaml"))
	if strings.Contains(k, "shop") || !strings.Contains(k, "resources:\n- notes.yaml\n") || !strings.Contains(k, "- path: notes.bundle.yaml") {
		t.Fatalf("kustomization:\n%s", k)
	}
	if !inRemote(remote, dt.CataloguePath("notes.yaml")) || !inRemote(remote, dt.CataloguePath(gitops.BundleFile("notes"))) {
		t.Fatal("another profile's files went with it")
	}
	subject := dt.Git(t, "", "--git-dir", remote, "log", "-1", "--format=%an|%s", "main")
	if !strings.Contains(subject, "Ada Lovelace|feat(catalogue): remove unused profile shop") {
		t.Fatalf("commit = %q", subject)
	}

	// Again: there is nothing to remove, and that is not a commit.
	tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")
	if res, err := g.RetireProfile(ctx, "shop", meta("u-ada")); err != nil || res.Status != "unchanged" || res.Changed {
		t.Fatalf("retire again = %+v, %v", res, err)
	}
	if dt.Git(t, "", "--git-dir", remote, "rev-parse", "main") != tip {
		t.Fatal("removing what is not there committed something")
	}

	// The last one out.
	if _, err := g.RetireProfile(ctx, "notes", meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	if got := dt.RemoteFile(t, remote, dt.CataloguePath("kustomization.yaml")); strings.TrimSpace(got) != strings.TrimSpace(emptyKustomization) {
		t.Fatalf("the kustomization with nothing left in it:\n%s", got)
	}
}

// A profile a tenant's manifest names stays: installed, switched on as an
// add-on, or pinned as one. So does one the platform ships.
func TestAProfileATenantNamesIsNotRetired(t *testing.T) {
	remote := dt.Remote(t, "demo", "solo")
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	digest := profilebundle.Digest([]byte(dt.ProfileYAML("deck")))
	if _, err := g.SetAddonsPinned(ctx, "solo", "nextcloud", []string{"calendar", "deck"},
		[]gitops.AddonPin{{Name: "deck", Digest: digest, Catalogue: "main"}}, meta("u-ada")); err != nil {
		t.Fatal(err)
	}
	tip := dt.Git(t, "", "--git-dir", remote, "rev-parse", "main")

	for name, why := range map[string]string{
		"nextcloud": "has it installed",
		"calendar":  "tenant solo has it switched on as an add-on of nextcloud",
		"deck":      "tenant solo has it switched on as an add-on of nextcloud",
		"desktop":   "the platform ships it",
	} {
		_, err := g.RetireProfile(ctx, name, meta("u-ada"))
		var inUse *gitops.ErrProfileInUse
		if !errors.As(err, &inUse) || !strings.Contains(inUse.Why, why) {
			t.Errorf("%s: %v, want in use because %q", name, err, why)
		}
	}
	if _, err := g.RetireProfile(ctx, "../demo", meta("u-ada")); !errors.Is(err, gitops.ErrInvalidName) {
		t.Errorf("a name that is not one: %v", err)
	}
	if dt.Git(t, "", "--git-dir", remote, "rev-parse", "main") != tip {
		t.Fatal("a refused removal committed something")
	}
	if !inRemote(remote, dt.CataloguePath("nextcloud.yaml")) || !inRemote(remote, dt.CataloguePath("calendar.yaml")) {
		t.Fatal("a profile in use was removed")
	}
}

// Which bundle file declares an object is read from the files themselves,
// by kind and name, and a file that cannot be read is not passed over.
func TestTheCatalogueDirectorySaysWhichBundleDeclaresAnObject(t *testing.T) {
	remote := dt.Remote(t, "demo")
	dt.Commit(t, remote, map[string]string{
		dt.CataloguePath("shop.yaml"): dt.ProfileYAML("shop") +
			"---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shop.theme\ndata: {}\n",
	})
	g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
	ctx := context.Background()
	for _, c := range []struct{ kind, name, want string }{
		{"ConfigMap", "shop.theme", "shop"},
		{"ComponentProfile", "shop", "shop"},
		{"Customization", "shop.theme", ""},
		{"ConfigMap", "shop.old-page", ""},
		{"Composition", "app-shop", ""},
	} {
		got, err := g.CatalogueDeclares(ctx, c.kind, c.name)
		if err != nil || got != c.want {
			t.Errorf("%s %s: declared by %q, %v; want %q", c.kind, c.name, got, err, c.want)
		}
	}

	dt.Commit(t, remote, map[string]string{dt.CataloguePath("broken.yaml"): "kind: [unclosed\n"})
	if got, err := g.CatalogueDeclares(ctx, "ConfigMap", "anything.at-all"); err == nil {
		t.Fatalf("a file that does not parse was passed over: declared by %q", got)
	}
}
