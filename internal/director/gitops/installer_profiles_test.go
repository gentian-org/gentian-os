/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// The installer places one profile before there is a director: the
// Operations Console, at step 0 (AD-14). It does so in shell
// (scripts/lib/catalogue.sh), and what it writes has to be what the director
// writes for an install of the same bundle -- or the cluster would hold two
// kinds of materialised profile, and the director's next install of the same
// entry would rewrite files nobody changed.
//
// These hold the two to each other, byte for byte, on the bundles
// gentian-apps publishes.

func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot tell where this test is")
	}
	return filepath.Join(filepath.Dir(here), "..", "..", "..")
}

// installerShell runs a line of shell with the installer's libraries loaded,
// the way the installer loads them.
func installerShell(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "yq", "openssl", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			// Somebody's laptop may lack one. The machine that always runs
			// this must not skip it.
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is not installed, and this comparison is not skipped in CI", tool)
			}
			t.Skipf("%s is not installed", tool)
		}
	}
	root := repoRoot(t)
	cmd := exec.Command("bash", append([]string{"-c",
		`source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1; trap - ERR; set +e; ` + script, "installer"}, args...)...)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "SCRIPT_DIR=" + root}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func builtBundles(t *testing.T) map[string][]byte {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "internal", "profilebundle", "testdata", "bundles")
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no built bundles in %s: %v", dir, err)
	}
	out := map[string][]byte{}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(filepath.Base(file), ".yaml")] = body
	}
	return out
}

// kustomizations a catalogue directory may start with: as step 0 scaffolds
// it, with entries of another profile, and one somebody added to by hand.
var startingKustomizations = map[string]string{
	"as scaffolded": emptyKustomization,
	"with another profile": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- other.yaml\n" +
		"patches:\n- path: other.bundle.yaml\n",
	"with another profile and a trailer": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n# Somebody's note.\n" +
		"resources:\n- other.yaml\n\npatches:\n- path: other.bundle.yaml\n  target:\n    kind: ComponentProfile\n\ncommonAnnotations:\n  example.org/x: \"1\"\n",
	"with resources only": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- other.yaml\n",
}

func TestTheInstallerMaterialisesAProfileAsTheDirectorDoes(t *testing.T) {
	origin := profilebundle.ClusterOrigin("gentian")
	for name, body := range builtBundles(t) {
		if len(body) > profilebundle.MaxBytes {
			continue
		}
		for label, start := range startingKustomizations {
			t.Run(name+"/"+label, func(t *testing.T) {
				// The director.
				remote := dt.Remote(t, "demo")
				dt.Commit(t, remote, map[string]string{dt.CataloguePath("kustomization.yaml"): start})
				g := gitops.NewGitOps(dt.Clone(t, remote), remote, dt.Cluster, director)
				if _, err := g.MaterialiseProfile(context.Background(), name, profilebundle.Digest(body), body, origin, meta("u-ada")); err != nil {
					t.Fatalf("the director refuses %s: %v", name, err)
				}

				// The installer.
				dir := t.TempDir()
				bundle := filepath.Join(t.TempDir(), "bundle.yaml")
				if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(start), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(bundle, body, 0o644); err != nil {
					t.Fatal(err)
				}
				run := func() {
					out, err := installerShell(t, `gentian_materialise_profile "$1" "$2" "$3" "$4"`, dir, name, bundle, origin)
					if err != nil {
						t.Fatalf("the installer: %v\n%s", err, out)
					}
				}
				run()
				compare := func(when string) {
					for _, file := range []string{name + ".yaml", gitops.BundleFile(name), "kustomization.yaml"} {
						want := rawRemoteFile(t, remote, dt.CataloguePath(file))
						got, err := os.ReadFile(filepath.Join(dir, file))
						if err != nil {
							t.Fatalf("%s: the installer wrote no %s: %v", when, file, err)
						}
						if !bytes.Equal(got, []byte(want)) {
							t.Errorf("%s: %s differs.\n--- the director\n%s\n--- the installer\n%s", when, file, clip(want), clip(string(got)))
						}
					}
				}
				compare("first write")

				// And what the installer wrote is, to the director, a profile
				// that is already there at that digest: nothing to commit.
				installed := dt.Remote(t, "demo")
				files := map[string]string{}
				for _, file := range []string{name + ".yaml", gitops.BundleFile(name), "kustomization.yaml"} {
					raw, err := os.ReadFile(filepath.Join(dir, file))
					if err != nil {
						t.Fatal(err)
					}
					files[dt.CataloguePath(file)] = string(raw)
				}
				dt.Commit(t, installed, files)
				after := gitops.NewGitOps(dt.Clone(t, installed), installed, dt.Cluster, director)
				have, err := after.ProfileOnCluster(context.Background(), name)
				if err != nil || !have.Present || have.Origin != origin || have.Digest != profilebundle.Digest(body) {
					t.Fatalf("the director reads the installer's profile as %+v, %v", have, err)
				}
				res, err := after.MaterialiseProfile(context.Background(), name, profilebundle.Digest(body), body, origin, meta("u-ada"))
				if err != nil || res.Status != "unchanged" || res.Changed {
					t.Fatalf("the director's install over the installer's files = %+v, %v; it should have nothing to change", res, err)
				}

				// A second run of the installer changes nothing either.
				run()
				compare("second write")
			})
		}
	}
}

// What the installer will place, the director accepts; and a file the
// director refuses for what it holds is not placed by the installer.
func TestTheInstallerRefusesWhatTheDirectorRefusesOfABundle(t *testing.T) {
	origin := profilebundle.ClusterOrigin("gentian")
	refusal := func(t *testing.T, name string, body []byte) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "bundle.yaml")
		if err := os.WriteFile(file, body, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := installerShell(t, `_profile_bundle_refusal "$1" "$2"`, file, name)
		if err != nil {
			t.Fatalf("the installer's check: %v\n%s", err, out)
		}
		return strings.TrimSpace(out)
	}

	for name, body := range builtBundles(t) {
		if _, err := profilebundle.Check(body, name, origin); err != nil {
			t.Fatalf("the director refuses the built bundle %s: %v", name, err)
		}
		if said := refusal(t, name, body); said != "" {
			t.Errorf("the installer refuses the built bundle %s, which the director accepts: %s", name, said)
		}
	}

	profile := dt.ProfileYAML("shop")
	companion := func(doc string) []byte { return []byte(profile + "---\n" + doc) }
	refused := map[string][]byte{
		"under another name": []byte(dt.ProfileYAML("other")),
		"not a profile":      []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shop\n"),
		"nothing":            []byte("# nothing here\n"),
		"states its own origin": []byte(strings.Replace(profile, "metadata:\n", "metadata:\n  annotations:\n    "+
			profilebundle.OriginAnnotation+": cluster/main\n", 1)),
		"states its own bundle": []byte(strings.Replace(profile, "metadata:\n", "metadata:\n  annotations:\n    "+
			profilebundle.Annotation+": \"eA==\"\n", 1)),
		"addressed to Argo CD": []byte(strings.Replace(profile, "metadata:\n", "metadata:\n  annotations:\n    "+
			"argocd.argoproj.io/sync-options: Replace=true\n", 1)),
		"a second profile":        companion(dt.ProfileYAML("shop2")),
		"a kind no bundle holds":  companion("apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: shop.all\n"),
		"the platform's own":      companion("apiVersion: apiextensions.crossplane.io/v1\nkind: Composition\nmetadata:\n  name: app-default\n  labels:\n    gentianos.io/profile-name: shop\nspec:\n  compositeTypeRef:\n    apiVersion: gentianos.io/v1alpha1\n    kind: XApp\n"),
		"another profile's name":  companion("apiVersion: gentianos.io/v1alpha1\nkind: OIDCPackCatalog\nmetadata:\n  name: other-oidc\n  labels:\n    gentianos.io/profile-name: shop\nspec:\n  packs: {}\n"),
		"states a namespace":      companion("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shop.files\n  namespace: kube-system\n  labels:\n    gentianos.io/profile-name: shop\n    gentianos.io/asset: files\ndata: {}\n"),
		"composes something else": companion("apiVersion: apiextensions.crossplane.io/v1\nkind: Composition\nmetadata:\n  name: app-shop\n  labels:\n    gentianos.io/profile-name: shop\nspec:\n  compositeTypeRef:\n    apiVersion: gentianos.io/v1alpha1\n    kind: XCluster\n"),
	}
	for what, body := range refused {
		if _, err := profilebundle.Check(body, "shop", origin); err == nil {
			// The two annotations are refused by the director where it
			// writes the patch, not in Check.
			if what != "states its own origin" && what != "states its own bundle" {
				t.Fatalf("%s: the director accepts it, so this is not a case of the two agreeing", what)
			}
		}
		if said := refusal(t, "shop", body); said == "" {
			t.Errorf("%s: the director refuses it and the installer would place it", what)
		}
	}
}

// rawRemoteFile is a file at the tip of the remote, every byte of it: the
// comparison is of bytes, a trailing newline included.
func rawRemoteFile(t *testing.T, remote, path string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", remote, "show", "main:"+path).Output()
	if err != nil {
		t.Fatalf("git show %s: %v", path, err)
	}
	return string(out)
}

func clip(s string) string {
	if len(s) > 1200 {
		return s[:600] + "\n[...]\n" + s[len(s)-600:]
	}
	return s
}
