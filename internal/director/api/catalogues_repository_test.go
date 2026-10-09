/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// A catalogue kept in the cluster's own deployments repository.
//
// The repository the fixture director writes is also what such a catalogue
// is read from: a directory holding index.yaml and profiles/<name>.yaml, as
// an address would serve them. The people are the fixture's: alice
// administers the cluster, tom administers demo, tina administers solo.

func pathBody(dir string) string { return fmt.Sprintf(`{"path":%q}`, dir) }

// inRepository is a catalogue's files below dir: its profiles, and an index
// that lists each at the digest of its bytes.
func inRepository(dir string, profiles map[string]string) map[string]string {
	files := map[string]string{}
	index := "entries:\n"
	for name, body := range profiles {
		files[dir+"/profiles/"+name+".yaml"] = body
		index += "  - name: " + name + "\n    version: \"1.0.0\"\n    edition: pe\n    digest: " + sha(body) + "\n"
	}
	files[dir+"/index.yaml"] = index
	return files
}

// startRepositoryCatalogues is the harness with files committed to the
// deployments repository before the director first reads it, and no
// catalogue at any address.
func startRepositoryCatalogues(t *testing.T, files ...map[string]string) *harness {
	t.Helper()
	return startSeeded(t, nil, func(remote string) {
		all := map[string]string{dt.ClaimPath: claimWith()}
		for _, set := range files {
			for path, body := range set {
				all[path] = body
			}
		}
		dt.Commit(t, remote, all)
	}, withFetcher(nil))
}

// commitLinks commits symbolic links into the remote: link path -> target.
func commitLinks(t *testing.T, remote string, links map[string]string) {
	t.Helper()
	work := t.TempDir()
	dt.Git(t, "", "clone", "--no-local", remote, work)
	for path, target := range links {
		full := filepath.Join(work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}
	dt.Git(t, work, "add", "-A")
	dt.Git(t, work, "-c", "user.name=seed", "-c", "user.email=seed@example.com", "commit", "-m", "links")
	dt.Git(t, work, "push", "origin", "HEAD:main")
}

// The cluster's administrator names a directory of the deployments
// repository as a catalogue of the whole cluster, and from then on it is a
// catalogue like any other: listed, browsed, and installed from at the digest
// its index states, with the profile committed to the cluster's catalogue
// directory and recorded as coming from that catalogue.
func TestACatalogueKeptInTheDeploymentsRepositoryIsInstalledFrom(t *testing.T) {
	app := profileNamed("planner", "own")
	h := startRepositoryCatalogues(t, inRepository("catalogue", map[string]string{"planner": app}))
	alice, tom, tina := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-solo", "tina")
	install := fmt.Sprintf(`{"coordinate":"own/planner","digest":%q}`, sha(app))

	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/planner", tom, install); code != http.StatusUnprocessableEntity {
		t.Fatalf("before the catalogue is declared: %d", code)
	}
	// A trailing slash is the same directory.
	code, out := h.do(t, "PUT", clusterCatalogues+"/own", alice, pathBody("catalogue/"))
	if code != http.StatusAccepted || out["scope"] != "cluster" {
		t.Fatalf("add: %d %v", code, out)
	}
	claim := dt.RemoteFile(t, h.remote, dt.ClaimPath)
	if !strings.Contains(claim, "    sources:\n      - name: own\n        path: catalogue") || strings.Contains(claim, "url:") {
		t.Fatalf("the claim:\n%s", claim)
	}
	// The same again changes nothing; another directory under the name is refused.
	tip := h.tip(t)
	if code, out := h.do(t, "PUT", clusterCatalogues+"/own", alice, pathBody("catalogue")); code != http.StatusOK || out["status"] != "unchanged" || h.tip(t) != tip {
		t.Fatalf("the same catalogue again: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", clusterCatalogues+"/own", alice, urlBody("https://catalogue.example.com")); code != http.StatusConflict {
		t.Fatalf("an address under a name a directory has: %d %v", code, out)
	}

	// Every tenant sees it, with where it is kept and no address.
	_, out = h.do(t, "GET", "/v1/tenants/solo/catalogues", tina, "")
	listed, _ := out["catalogues"].([]any)
	if len(listed) != 1 {
		t.Fatalf("solo sees %v", out)
	}
	if c := listed[0].(map[string]any); c["name"] != "own" || c["path"] != "catalogue" || c["url"] != "" || c["scope"] != "cluster" || c["addedBy"] != "cluster" {
		t.Fatalf("solo sees %v", c)
	}
	_, out = h.do(t, "GET", clusterCatalogues, alice, "")
	if c := out["catalogues"].([]any)[0].(map[string]any); c["path"] != "catalogue" {
		t.Fatalf("the cluster's list: %v", out)
	}
	// Its index is read, and states the digest an install is pinned to.
	code, out = h.do(t, "GET", "/v1/tenants/demo/catalogues/own/entries", tom, "")
	entries, _ := out["entries"].([]any)
	if code != http.StatusOK || len(entries) != 1 {
		t.Fatalf("entries: %d %v", code, out)
	}
	if e := entries[0].(map[string]any); e["coordinate"] != "own/planner" || e["digest"] != sha(app) || e["installable"] != true {
		t.Fatalf("the entry: %v", e)
	}

	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/planner", tom, install); code != http.StatusAccepted {
		t.Fatalf("install: %d %v", code, out)
	}
	// Materialised where every profile is, byte for byte, with its origin.
	if got := dt.RemoteFile(t, h.remote, dt.CataloguePath("planner.yaml")); got != strings.TrimSpace(app) {
		t.Fatalf("the materialised profile:\n%s", got)
	}
	bundle := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BundleFile("planner")))
	if !strings.Contains(bundle, profilebundle.OriginAnnotation+": cluster/own") {
		t.Fatalf("the profile's origin:\n%s", bundle)
	}
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(manifest, "profile: planner") {
		t.Fatalf("the tenant's manifest:\n%s", manifest)
	}
	// Removed like any other; what was installed stays.
	if code, _ := h.do(t, "DELETE", clusterCatalogues+"/own", alice, ""); code != http.StatusAccepted {
		t.Fatal("remove")
	}
	if _, out := h.do(t, "GET", "/v1/tenants/solo/catalogues", tina, ""); names(out) != "" {
		t.Fatalf("after removing: %v", out)
	}
}

// For one tenant, it is that tenant's alone: no other tenant sees it or
// installs from it, and what is installed from it is recorded as the
// tenant's -- exactly as a catalogue at an address the cluster's
// administrator added for the tenant.
func TestARepositoryCatalogueForOneTenantIsThatTenantsAlone(t *testing.T) {
	app := profileNamed("demo-planner", "own")
	h := startRepositoryCatalogues(t, inRepository("catalogues/demo", map[string]string{"demo-planner": app}))
	alice, tom, tina := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-solo", "tina")
	install := fmt.Sprintf(`{"coordinate":"own/demo-planner","digest":%q}`, sha(app))

	code, out := h.do(t, "PUT", clusterTenant("demo", "/catalogues/own"), alice, pathBody("catalogues/demo"))
	if code != http.StatusAccepted || out["scope"] != "tenant" || out["tenant"] != "demo" {
		t.Fatalf("add: %d %v", code, out)
	}
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(manifest, "  catalogue:\n    sources:\n      - name: own\n        path: catalogues/demo\n        addedBy: cluster") {
		t.Fatalf("the manifest:\n%s", manifest)
	}
	if _, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", tom, ""); names(out) != "own:tenant:cluster" {
		t.Fatalf("demo sees %v", out)
	}
	if _, out := h.do(t, "GET", "/v1/tenants/solo/catalogues", tina, ""); names(out) != "" {
		t.Fatalf("solo sees %v", out)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/solo/apps/demo-planner", tina, install); code != http.StatusUnprocessableEntity {
		t.Fatalf("another tenant installed from it: %d", code)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/demo-planner", tom, install); code != http.StatusAccepted {
		t.Fatalf("install: %d %v", code, out)
	}
	bundle := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BundleFile("demo-planner")))
	if !strings.Contains(bundle, profilebundle.OriginAnnotation+": tenant/demo/own") {
		t.Fatalf("the profile's origin:\n%s", bundle)
	}
	// The tenant's administrator does not remove what the cluster added.
	delegate(t, h, "demo")
	if code, _ := h.do(t, "DELETE", "/v1/tenants/demo/catalogues/own", tom, ""); code != http.StatusForbidden {
		t.Fatalf("the tenant removed the cluster's catalogue: %d", code)
	}
}

// It is held to what a catalogue at an address is held to, and to no less
// for being in the cluster's own repository: bytes that are not the digest
// the install named are refused, and a bundle from a tenant's catalogue
// brings no companion.
func TestARepositoryCatalogueIsHeldToTheDigestAndTheBundleChecks(t *testing.T) {
	app := profileNamed("planner", "own")
	composition := "---\napiVersion: apiextensions.crossplane.io/v1\nkind: Composition\nmetadata:\n  name: app-demo-notes\n" +
		"  labels:\n    gentianos.io/profile-name: demo-notes\nspec:\n  compositeTypeRef: {apiVersion: gentianos.io/v1alpha1, kind: XApp}\n"
	withCompanion := profileNamed("demo-notes", "own") + composition
	h := startRepositoryCatalogues(t,
		inRepository("catalogue", map[string]string{"planner": app}),
		inRepository("catalogues/demo", map[string]string{"demo-notes": withCompanion}))
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "PUT", clusterCatalogues+"/own", alice, pathBody("catalogue")); code != http.StatusAccepted {
		t.Fatalf("add: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", clusterTenant("demo", "/catalogues/theirs"), alice, pathBody("catalogues/demo")); code != http.StatusAccepted {
		t.Fatalf("add for the tenant: %d %v", code, out)
	}
	before := h.tip(t)

	other := fmt.Sprintf(`{"coordinate":"own/planner","digest":%q}`, sha(app+"# changed\n"))
	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/planner", tom, other)
	if code != http.StatusBadGateway || !strings.Contains(fmt.Sprint(out["error"]), "is not this entry") {
		t.Fatalf("a digest the file does not hash to: %d %v", code, out)
	}
	withIt := fmt.Sprintf(`{"coordinate":"theirs/demo-notes","digest":%q}`, sha(withCompanion))
	code, out = h.do(t, "POST", "/v1/tenants/demo/apps/demo-notes", tom, withIt)
	if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), "not a bundle this cluster installs") {
		t.Fatalf("a companion from a tenant's repository catalogue: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}
}

// What is declared is a directory of the repository and nothing else: no
// way up or out, no other repository or host, no hidden directory, and never
// the directory the director writes installed profiles into. Refused on both
// of the cluster administrator's routes, and nothing is committed.
func TestADirectoryThatIsNotACataloguesIsRefused(t *testing.T) {
	h := startRepositoryCatalogues(t, inRepository("catalogue", map[string]string{"planner": profileNamed("planner", "own")}))
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)
	for _, dir := range []string{
		"..",
		"../other-repository",
		"catalogue/..",
		"catalogue/../..",
		"catalogue/../../../etc",
		"/etc",
		"/catalogue",
		"//catalogue",
		"catalogue//profiles",
		".",
		"./catalogue",
		".git",
		"catalogue/.hidden",
		`catalogue\..`,
		"cata logue",
		"catalogue\n",
		"catalogue#x",
		"git@example.com:other/repository.git",
		"file:///etc",
		"ssh://git.example.com/other.git",
		"example.com:catalogue",
		"%2e%2e/x",
		// Where the director materialises profiles: this cluster's, another
		// cluster's of the same repository, and whatever the case.
		"clusters/" + dt.Cluster + "/catalogue",
		"clusters/" + dt.Cluster + "/catalogue/",
		"clusters/" + dt.Cluster + "/catalogue/sub",
		"clusters/another-cluster/catalogue",
		"Clusters/" + dt.Cluster + "/Catalogue",
		// Written well, and not there.
		"no-such-directory",
		"catalogue/index.yaml", // a file
		strings.Repeat("a", 256),
	} {
		for _, route := range []string{clusterCatalogues + "/own", clusterTenant("demo", "/catalogues/own")} {
			code, out := h.do(t, "PUT", route, alice, pathBody(dir))
			if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), "not one a catalogue is read from") {
				t.Errorf("%q on %s: %d %v, want 422", dir, route, code, out)
			}
		}
	}
	// One or the other, never both and never neither.
	for _, body := range []string{`{"path":""}`, `{"path":"catalogue","url":"https://catalogue.example.com"}`, `{"path":["catalogue"]}`} {
		if code, _ := h.do(t, "PUT", clusterCatalogues+"/own", alice, body); code != http.StatusBadRequest {
			t.Errorf("body %s: %d, want 400", body, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused directory moved the repository")
	}
}

// A symbolic link committed to the repository is not followed: not as the
// catalogue's directory, not on the way to it, and not as one of its files --
// whether it points out of the repository or at another part of it.
func TestASymbolicLinkInTheRepositoryIsNotFollowed(t *testing.T) {
	app := profileNamed("planner", "own")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(filepath.Join(outside, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "profiles", "planner.yaml"), []byte(app), 0o644); err != nil {
		t.Fatal(err)
	}
	h := startSeeded(t, nil, func(remote string) {
		files := inRepository("catalogue", map[string]string{"planner": app})
		files[dt.ClaimPath] = claimWith()
		delete(files, "catalogue/profiles/planner.yaml")
		// catalogue/ is a real directory whose one profile is a link.
		files["catalogue/profiles/.keep"] = ""
		dt.Commit(t, remote, files)
		commitLinks(t, remote, map[string]string{
			"escape":                          outside,
			"materialised":                    "clusters/" + dt.Cluster + "/catalogue",
			"linked/inner":                    outside,
			"catalogue/profiles/planner.yaml": filepath.Join(outside, "profiles", "planner.yaml"),
		})
	}, withFetcher(nil))
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	for _, dir := range []string{"escape", "materialised", "linked/inner", "escape/profiles"} {
		code, out := h.do(t, "PUT", clusterCatalogues+"/own", alice, pathBody(dir))
		if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), "symbolic link") {
			t.Errorf("%s: %d %v, want 422 naming the link", dir, code, out)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused directory moved the repository")
	}
	// A directory that is one, holding a file that is a link out of the
	// repository to the very bytes the digest names.
	if code, out := h.do(t, "PUT", clusterCatalogues+"/own", alice, pathBody("catalogue")); code != http.StatusAccepted {
		t.Fatalf("add: %d %v", code, out)
	}
	before = h.tip(t)
	install := fmt.Sprintf(`{"coordinate":"own/planner","digest":%q}`, sha(app))
	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/planner", tom, install)
	if code != http.StatusBadGateway || !strings.Contains(fmt.Sprint(out["error"]), "symbolic link") {
		t.Fatalf("a profile that is a link: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}
}

// A tenant's administrator does not name a directory of the deployments
// repository, delegated or not: the repository is the cluster
// administrator's. Refused before anything is read, and nothing is
// committed. An address they may still add where that is delegated.
func TestATenantsAdministratorAddsNoRepositoryCatalogue(t *testing.T) {
	h := startRepositoryCatalogues(t, inRepository("catalogue", map[string]string{"planner": profileNamed("planner", "own")}))
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/own", tom, pathBody("catalogue")); code != http.StatusForbidden {
		t.Fatalf("not delegated: %d %v, want 403", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
	delegate(t, h, "demo")
	before = h.tip(t)
	code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/own", tom, pathBody("catalogue"))
	if code != http.StatusForbidden || !strings.Contains(fmt.Sprint(out["error"]), "added by the cluster's administrator") {
		t.Fatalf("delegated: %d %v, want 403 saying whose it is", code, out)
	}
	// Nor on the cluster's routes, which are not theirs at all.
	for _, route := range []string{clusterCatalogues + "/own", clusterTenant("demo", "/catalogues/own")} {
		if code, _ := h.do(t, "PUT", route, tom, pathBody("catalogue")); code != http.StatusForbidden {
			t.Fatalf("%s as the tenant's administrator: %d", route, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
	if _, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", tom, ""); names(out) != "" {
		t.Fatalf("after the refusals demo sees %v", out)
	}
	// A manifest that says a tenant added one -- written by hand, since no
	// route writes it -- is not read from.
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	edited := strings.Replace(manifest+"\n", "    delegated: true\n",
		"    delegated: true\n    sources:\n      - name: own\n        path: catalogue\n        addedBy: tenant\n", 1)
	if !strings.Contains(edited, "addedBy: tenant") {
		t.Fatalf("the manifest was not edited:\n%s", manifest)
	}
	dt.Commit(t, h.remote, map[string]string{dt.TenantPath("demo"): edited})
	alice := h.token(t, "gentian", "alice")
	if code, out := h.do(t, "PUT", clusterTenant("solo", "/catalogue-delegation"), alice, ""); code != http.StatusAccepted {
		t.Fatalf("a write, so that the director reads the repository again: %d %v", code, out)
	}
	if _, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", tom, ""); names(out) != "" {
		t.Fatalf("a hand-written tenant entry with a path is read: %v", out)
	}
}

// A claim somebody edited by hand to name the directory installed profiles
// are written into, or a way out of the repository, declares no catalogue.
func TestAClaimNamingADirectoryNoCatalogueIsReadFromDeclaresNone(t *testing.T) {
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath: claimWith() + "  catalogue:\n    sources:\n" +
				"      - name: installed\n        path: clusters/" + dt.Cluster + "/catalogue\n" +
				"      - name: up\n        path: ../elsewhere\n" +
				"      - name: both\n        path: catalogue\n        url: https://catalogue.example.com\n" +
				"      - name: own\n        path: catalogue\n",
		})
		dt.Commit(t, remote, inRepository("catalogue", map[string]string{"planner": profileNamed("planner", "own")}))
	}, withFetcher(nil))
	tom := h.token(t, "tenant-demo", "tom")
	if _, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", tom, ""); names(out) != "own:cluster:cluster" {
		t.Fatalf("demo sees %v", out)
	}
	// Nextcloud is in the directory installed profiles are written into. It
	// is not offered from there, and not installed from there.
	if code, _ := h.do(t, "GET", "/v1/tenants/demo/catalogues/installed/entries", tom, ""); code != http.StatusNotFound {
		t.Fatalf("the installed profiles were listed as a catalogue: %d", code)
	}
	nextcloud := dt.RemoteFile(t, h.remote, dt.CataloguePath("nextcloud.yaml")) + "\n"
	install := fmt.Sprintf(`{"coordinate":"installed/nextcloud","digest":%q}`, sha(nextcloud))
	if code, _ := h.do(t, "POST", "/v1/tenants/solo/apps/nextcloud", h.token(t, "tenant-solo", "tina"), install); code != http.StatusUnprocessableEntity {
		t.Fatalf("an install from the directory installed profiles are written into: %d", code)
	}
}
