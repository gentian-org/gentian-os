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
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

// A catalogue is never kept in the cluster's deployments repository, which
// holds the cluster's state only. The people are the fixture's: alice
// administers the cluster, tom administers demo.

// inRepository is a catalogue's files below dir, as an address would serve
// them: its profiles, and an index that lists each at the digest of its bytes.
func inRepository(dir string, profiles map[string]string) map[string]string {
	files := map[string]string{}
	index := "entries:\n"
	for name, body := range profiles {
		files[dir+"/profiles/"+name+".yaml"] = body
		index += "  - name: " + name + "\n    version: \"1.0.0\"\n    edition: ie\n    digest: " + sha(body) + "\n"
	}
	files[dir+"/index.yaml"] = index
	return files
}

// A request that names a directory of the deployments repository (path) in
// place of an address or an artifact is refused on every route a catalogue
// is added on, saying why, and nothing is committed.
func TestNoCatalogueIsAddedAsADirectoryOfTheDeploymentsRepository(t *testing.T) {
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, inRepository("catalogue", map[string]string{"planner": profileNamed("planner", "own")}))
	}, withFetcher(nil))
	alice := h.token(t, "gentian", "alice")
	tom := h.token(t, "tenant-demo", "tom")
	delegate(t, h, "demo")
	before := h.tip(t)
	for _, c := range []struct{ route, token string }{
		{clusterCatalogues + "/own", alice},
		{clusterTenant("demo", "/catalogues/own"), alice},
		{"/v1/tenants/demo/catalogues/own", tom},
	} {
		for _, body := range []string{`{"path":"catalogue"}`, `{"path":"catalogue","url":"https://catalogue.example.com"}`} {
			code, out := h.do(t, "PUT", c.route, c.token, body)
			if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "never kept in the deployments repository") {
				t.Errorf("%s on %s: %d %v, want 400 saying why", body, c.route, code, out)
			}
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
}

// A claim or a tenant's manifest somebody edited by hand to name a directory
// of the deployments repository declares no catalogue there, even where the
// directory holds one; what else it declares is read as before.
func TestAClaimOrManifestNamingADirectoryDeclaresNoCatalogue(t *testing.T) {
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath: claimWith() + "  catalogue:\n    sources:\n" +
				"      - name: own\n        path: catalogue\n" +
				"      - name: installed\n        path: clusters/" + dt.Cluster + "/catalogue\n" +
				"      - name: address\n        url: https://catalogue.example.com\n",
		})
		dt.Commit(t, remote, inRepository("catalogue", map[string]string{"planner": profileNamed("planner", "own")}))
	}, withFetcher(nil))
	delegate(t, h, "demo")
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	edited := strings.Replace(manifest+"\n", "    delegated: true\n",
		"    delegated: true\n    sources:\n      - name: mine\n        path: catalogue\n        addedBy: cluster\n", 1)
	if !strings.Contains(edited, "name: mine") {
		t.Fatalf("the manifest was not edited:\n%s", manifest)
	}
	dt.Commit(t, h.remote, map[string]string{dt.TenantPath("demo"): edited})
	alice := h.token(t, "gentian", "alice")
	if code, out := h.do(t, "PUT", clusterTenant("solo", "/catalogue-delegation"), alice, ""); code != http.StatusAccepted {
		t.Fatalf("a write, so that the director reads the repository again: %d %v", code, out)
	}
	tom := h.token(t, "tenant-demo", "tom")
	if _, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", tom, ""); names(out) != "address:cluster:cluster" {
		t.Fatalf("demo sees %v", out)
	}
	for _, name := range []string{"own", "installed", "mine"} {
		if code, _ := h.do(t, "GET", "/v1/tenants/demo/catalogues/"+name+"/entries", tom, ""); code != http.StatusNotFound {
			t.Errorf("the directory named by %s was listed as a catalogue: %d", name, code)
		}
	}
}
