/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// Catalogues kept as OCI artifacts, read with the credential of a
// repository the catalogue's owner declared. One registry plays every
// registry host (directortest.Registry); the vault is a map, and records
// every path it is asked for.
//
// The people are the fixture's: alice administers the cluster (and demo, not
// solo), tom administers demo, tina administers solo.

type fakeVault struct {
	mu    sync.Mutex
	data  map[string]map[string]string
	asked []string
}

func (v *fakeVault) Get(_ context.Context, path string) (map[string]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.asked = append(v.asked, path)
	if d, ok := v.data[path]; ok {
		return d, nil
	}
	return nil, errors.New("not found")
}

func (v *fakeVault) paths() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.asked...)
}

// withRegistry is withFetcher for catalogue artifacts: the fetcher reaches
// the fake registry, and reads repositories as cmd/director wires it, from
// the director's own repository and the vault.
func withRegistry(reg *dt.Registry, vault *fakeVault) func(*api.Config) {
	return func(cfg *api.Config) {
		withFetcher(nil)(cfg)
		cfg.Catalogue.Client = reg.Client()
		repo := cfg.Repo.(*gitops.GitOps)
		cfg.Catalogue.Registries = catalogue.VaultRegistries{Declared: repo.RegistryRepository, Read: vault.Get}
	}
}

func ociBody(repository, artifact string) string {
	return fmt.Sprintf(`{"oci":{"repository":%q,"artifact":%q}}`, repository, artifact)
}

func declare(t *testing.T, h *harness, path, token, url string) {
	t.Helper()
	if code, out := h.do(t, "PUT", path, token, fmt.Sprintf(`{"role":"apps","type":"oci","url":%q}`, url)); code != http.StatusAccepted {
		t.Fatalf("declare %s: %d %v", path, code, out)
	}
}

// startOCI is the harness with three repositories declared: demo's acme,
// solo's theirs, and the cluster's ours.
func startOCI(t *testing.T, reg *dt.Registry, vault *fakeVault) *harness {
	t.Helper()
	h := startWith(t, nil, withRegistry(reg, vault))
	declare(t, h, "/v1/tenants/demo/repositories/acme", h.token(t, "tenant-demo", "tom"), "oci://registry.example.com/acme")
	declare(t, h, "/v1/tenants/solo/repositories/theirs", h.token(t, "tenant-solo", "tina"), "oci://registry.example.com/solo")
	declare(t, h, "/v1/clusters/"+dt.Cluster+"/repositories/ours", h.token(t, "gentian", "alice"), "oci://registry.example.com/ours")
	return h
}

// Who may add an artifact is who may add an address; with which repository
// it is read is where it is added. A tenant's catalogue is read with a
// repository of that tenant's, and a catalogue of the cluster with one of
// the cluster's, whoever adds it -- every other name is a repository that is
// not declared, in the words one nobody declared is refused in.
func TestAnArtifactIsAddedWithItsOwnersRepositoryOnly(t *testing.T) {
	vault := &fakeVault{}
	h := startOCI(t, dt.NewRegistry(t), vault)
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	demoArtifact := "registry.example.com/acme/catalogue:1"

	// Not delegated: a tenant's administrator is refused, before anything
	// is looked up.
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/acme", tom, ociBody("acme", demoArtifact)); code != http.StatusForbidden {
		t.Fatalf("not delegated: %d %v", code, out)
	}
	delegate(t, h, "demo")

	for _, c := range []struct {
		what, path, token, body string
		want                    int
		says                    string
	}{
		{"another tenant's repository", "/v1/tenants/demo/catalogues/x", tom, ociBody("theirs", "registry.example.com/solo/catalogue:1"),
			http.StatusUnprocessableEntity, "not declared for tenant demo"},
		{"the cluster's repository, for a tenant", "/v1/tenants/demo/catalogues/x", tom, ociBody("ours", "registry.example.com/ours/catalogue:1"),
			http.StatusUnprocessableEntity, "not declared for tenant demo"},
		{"the cluster's repository, by the cluster's administrator for a tenant", clusterTenant("demo", "/catalogues/x"), alice,
			ociBody("ours", "registry.example.com/ours/catalogue:1"), http.StatusUnprocessableEntity, "not declared for tenant demo"},
		{"a tenant's repository, for the cluster", clusterCatalogues + "/x", alice, ociBody("acme", demoArtifact),
			http.StatusUnprocessableEntity, "not declared for the cluster"},
		{"a repository nobody declared", "/v1/tenants/demo/catalogues/x", tom, ociBody("absent", demoArtifact),
			http.StatusUnprocessableEntity, "not declared"},
		{"an artifact outside its repository", "/v1/tenants/demo/catalogues/x", tom, ociBody("acme", "registry.example.com/other/catalogue:1"),
			http.StatusUnprocessableEntity, "not inside the repository"},
		{"an artifact on another registry", "/v1/tenants/demo/catalogues/x", tom, ociBody("acme", "elsewhere.example.com/acme/catalogue:1"),
			http.StatusUnprocessableEntity, "its own registry only"},
		{"an artifact on a private address", "/v1/tenants/demo/catalogues/x", tom, ociBody("acme", "10.0.0.1/acme/catalogue:1"),
			http.StatusUnprocessableEntity, "10.0.0.0/8"},
		{"an artifact inside the cluster", "/v1/tenants/demo/catalogues/x", tom, ociBody("acme", "zot.kernel-gitops.svc/acme/catalogue:1"),
			http.StatusUnprocessableEntity, ".svc"},
		{"an artifact with no tag", "/v1/tenants/demo/catalogues/x", tom, ociBody("acme", "registry.example.com/acme/catalogue"),
			http.StatusUnprocessableEntity, "neither a tag nor a digest"},
		{"an artifact and an address", "/v1/tenants/demo/catalogues/x", tom,
			`{"url":"https://x.example.com","oci":{"repository":"acme","artifact":"registry.example.com/acme/c:1"}}`, http.StatusBadRequest, "body must be"},
	} {
		code, out := h.do(t, "PUT", c.path, c.token, c.body)
		if code != c.want || !strings.Contains(fmt.Sprint(out["error"]), c.says) {
			t.Errorf("%s: %d %v, want %d saying %q", c.what, code, out, c.want, c.says)
		}
	}
	// The credential is not asked for when a catalogue is added.
	if asked := vault.paths(); len(asked) != 0 {
		t.Fatalf("the vault was asked for %v", asked)
	}

	// Where they may: the tenant's own administrator, delegated, with the
	// tenant's own repository; the cluster's, with the cluster's.
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/acme", tom, ociBody("acme", demoArtifact)); code != http.StatusAccepted {
		t.Fatalf("demo's own: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", clusterCatalogues+"/ours", alice, ociBody("ours", "registry.example.com/ours/catalogue:1")); code != http.StatusAccepted {
		t.Fatalf("the cluster's own: %d %v", code, out)
	}
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(manifest, "        oci:\n          repository: acme\n          artifact: "+demoArtifact+"\n        addedBy: tenant") {
		t.Fatalf("demo's manifest:\n%s", manifest)
	}
	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", tom, "")
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(out["catalogues"]), "artifact:"+demoArtifact) {
		t.Fatalf("demo's list: %d %v", code, out)
	}
	// solo is not delegated, and sees neither demo's catalogue nor could use
	// demo's repository.
	tina := h.token(t, "tenant-solo", "tina")
	if code, _ := h.do(t, "PUT", "/v1/tenants/solo/catalogues/acme", tina, ociBody("acme", demoArtifact)); code != http.StatusForbidden {
		t.Fatalf("solo, not delegated: %d", code)
	}
	if code, out := h.do(t, "GET", "/v1/tenants/solo/catalogues", tina, ""); code != http.StatusOK || strings.Contains(fmt.Sprint(out), "acme") {
		t.Fatalf("solo's list: %d %v", code, out)
	}
}

// What an artifact holds is listed and installed from as what an address
// serves: pinned to the digest, recorded as the tenant's own catalogue, read
// with the tenant's credential and nobody else's.
func TestATenantInstallsFromItsOwnArtifact(t *testing.T) {
	app := profileNamed("demo-notes", "acme")
	reg := dt.NewRegistry(t)
	reg.Login = [2]string{"demo-bot", "demo-secret"}
	reg.Realm = "https://registry.example.com/token"
	reg.Token = "pull-token"
	reg.BlobsAt = "storage.example.net"
	reg.Push(t, "registry.example.com", "acme/catalogue", "1", dt.CatalogueLayer(t, map[string]string{
		"index.yaml":               "entries:\n  - name: demo-notes\n    version: \"1.0.0\"\n    edition: pe\n    digest: " + sha(app) + "\n",
		"profiles/demo-notes.yaml": app,
	}))
	vault := &fakeVault{data: map[string]map[string]string{
		"gentian-os/tenants/demo/repositories/acme": {"username": "demo-bot", "password": "demo-secret"},
		"gentian-os/tenants/solo/repositories/acme": {"username": "solo-bot", "password": "solo-secret"},
	}}
	h := startOCI(t, reg, vault)
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "PUT", clusterTenant("demo", "/catalogues/acme"), alice, ociBody("acme", "registry.example.com/acme/catalogue:1")); code != http.StatusAccepted {
		t.Fatalf("add: %d %v", code, out)
	}

	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues/acme/entries", tom, "")
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(out["entries"]), "coordinate:acme/demo-notes") {
		t.Fatalf("entries: %d %v", code, out)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/demo-notes", tom, fmt.Sprintf(`{"coordinate":"acme/demo-notes","digest":%q}`, sha(app))); code != http.StatusAccepted {
		t.Fatalf("install: %d %v", code, out)
	}
	bundle := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BundleFile("demo-notes")))
	if !strings.Contains(bundle, profilebundle.OriginAnnotation+": tenant/demo/acme") {
		t.Fatalf("the profile is not recorded as demo's acme:\n%s", bundle)
	}
	for _, path := range vault.paths() {
		if path != "gentian-os/tenants/demo/repositories/acme" {
			t.Fatalf("the vault was asked for %s", path)
		}
	}
	for _, req := range reg.Requests() {
		if strings.Contains(req.Authorization, "solo") || (req.Host == "storage.example.net" && req.Authorization != "") {
			t.Fatalf("%s was sent %q", req.Host, req.Authorization)
		}
	}
	// A build the artifact does not hold is refused like any other.
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/demo-notes", tom, fmt.Sprintf(`{"coordinate":"acme/demo-notes","digest":%q}`, sha("other"))); code < 400 {
		t.Fatalf("another digest: %d", code)
	}
}

// A catalogue added when the repository was declared is not read with
// another once the declaration changes owner: the repository is asked for on
// every pull, of the owner the catalogue is declared for.
func TestAnArtifactIsReadWithItsRepositoryOnEveryPull(t *testing.T) {
	reg := dt.NewRegistry(t)
	reg.Login = [2]string{"u", "p"}
	vault := &fakeVault{data: map[string]map[string]string{"gentian-os/tenants/demo/repositories/acme": {"username": "u", "password": "p"}}}
	h := startOCI(t, reg, vault)
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "PUT", clusterTenant("demo", "/catalogues/acme"), alice, ociBody("acme", "registry.example.com/acme/catalogue:1")); code != http.StatusAccepted {
		t.Fatalf("add: %d %v", code, out)
	}
	if code, out := h.do(t, "DELETE", "/v1/tenants/demo/repositories/acme?confirm=acme", tom, ""); code != http.StatusAccepted {
		t.Fatalf("remove the repository: %d %v", code, out)
	}
	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues/acme/entries", tom, "")
	if code != http.StatusBadGateway {
		t.Fatalf("entries without the repository: %d %v", code, out)
	}
	if len(vault.paths()) != 0 || len(reg.Requests()) != 0 {
		t.Fatalf("asked the vault %v and the registry %v", vault.paths(), reg.Requests())
	}
}
