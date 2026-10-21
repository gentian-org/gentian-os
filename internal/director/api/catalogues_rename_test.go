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
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// A catalogue is renamed by the cluster's administrator, as one commit to
// the source entry, the entries installed from it and the profiles' origin;
// the next install from the new name is the same build again.
func TestACatalogueIsRenamedWithWhatWasInstalledFromIt(t *testing.T) {
	planner, ledger := profileNamed("planner", "partner"), profileNamed("ledger", "ours")
	src := catalogues(t, served{"gentian": {}, "partner": {"planner": planner}, "ours": {"ledger": ledger}})
	h := startCatalogues(t, src)
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	cluster := "cluster:" + dt.Cluster

	// One of the cluster's, one of demo's, each with an install from it.
	if code, out := h.do(t, "PUT", clusterCatalogues+"/partner", alice, urlBody(src.URL+"/partner")); code != http.StatusAccepted {
		t.Fatalf("add partner: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", clusterTenant("demo", "/catalogues/ours"), alice, urlBody(src.URL+"/ours")); code != http.StatusAccepted {
		t.Fatalf("add ours: %d %v", code, out)
	}
	install := func(coordinate, body string) string {
		return fmt.Sprintf(`{"coordinate":%q,"digest":%q}`, coordinate, sha(body))
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/planner", tom, install("partner/planner", planner)); code != http.StatusAccepted {
		t.Fatalf("install planner: %d %v", code, out)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/ledger", tom, install("ours/ledger", ledger)); code != http.StatusAccepted {
		t.Fatalf("install ledger: %d %v", code, out)
	}

	// The cluster's: asked of the cluster, one commit by the person.
	before := h.tip(t)
	h.asked.reset()
	code, out := h.do(t, "POST", clusterCatalogues+"/partner/actions/rename", alice, `{"name":"acme"}`)
	if code != http.StatusAccepted || out["status"] != "renamed" || out["name"] != "partner" || out["newName"] != "acme" || out["scope"] != "cluster" || out["commit"] == "" {
		t.Fatalf("rename: %d %v", code, out)
	}
	if asked := h.asked.questions(); len(asked) != 1 || asked[0] != "user:alice can_configure "+cluster {
		t.Fatalf("asked %v", asked)
	}
	if dt.Git(t, "", "--git-dir", h.remote, "rev-parse", "main~1") != before {
		t.Fatal("more than one commit")
	}
	if subject := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an|%s", "main"); !strings.Contains(subject, "Alice|feat(cluster): rename catalogue partner to acme") {
		t.Fatalf("commit = %q", subject)
	}
	if claim := dt.RemoteFile(t, h.remote, dt.ClaimPath); !strings.Contains(claim, "      - name: acme\n        url: "+src.URL+"/partner") || strings.Contains(claim, "name: partner") {
		t.Fatalf("the claim:\n%s", claim)
	}
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(manifest, "  - profile: planner\n    digest: "+sha(planner)+"\n    catalogue: acme\n") || !strings.Contains(manifest, "    catalogue: ours\n") {
		t.Fatalf("demo's manifest:\n%s", manifest)
	}
	if bundle := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BundleFile("planner"))); !strings.Contains(bundle, profilebundle.OriginAnnotation+": cluster/acme") {
		t.Fatalf("planner's origin:\n%s", bundle)
	}
	if build := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BuildsDir+"/"+gitops.BuildFile("planner", sha(planner)))); !strings.Contains(build, "\n  origin: cluster/acme\n") {
		t.Fatalf("planner's build:\n%s", build)
	}
	// What the tenant sees, and what it installs from, is the new name;
	// the same build from it is what is already installed.
	if _, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", tom, ""); names(out) != "gentian:cluster:cluster acme:cluster:cluster ours:tenant:cluster" {
		t.Fatalf("demo sees %v", out)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/planner", tom, install("acme/planner", planner)); code != http.StatusOK || out["status"] != "already_installed" {
		t.Fatalf("install from acme: %d %v", code, out)
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/planner", tom, install("partner/planner", planner)); code != http.StatusUnprocessableEntity {
		t.Fatalf("install from the old name: %d", code)
	}

	// The tenant's: the same, under the tenant's route of the cluster.
	code, out = h.do(t, "POST", clusterTenant("demo", "/catalogues/ours/actions/rename"), alice, `{"name":"aluvian-team"}`)
	if code != http.StatusAccepted || out["status"] != "renamed" || out["scope"] != "tenant" || out["tenant"] != "demo" {
		t.Fatalf("rename ours: %d %v", code, out)
	}
	manifest = dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	if !strings.Contains(manifest, "      - name: aluvian-team\n        url: "+src.URL+"/ours\n        addedBy: cluster") || !strings.Contains(manifest, "    catalogue: aluvian-team\n") || strings.Contains(manifest, "name: ours") || strings.Contains(manifest, "catalogue: ours") {
		t.Fatalf("demo's manifest:\n%s", manifest)
	}
	if bundle := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BundleFile("ledger"))); !strings.Contains(bundle, profilebundle.OriginAnnotation+": tenant/demo/aluvian-team") {
		t.Fatalf("ledger's origin:\n%s", bundle)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/ledger", tom, install("aluvian-team/ledger", ledger)); code != http.StatusOK || out["status"] != "already_installed" {
		t.Fatalf("install from aluvian-team: %d %v", code, out)
	}
}

// Nobody but the cluster's administrator renames, on either route; and a
// rename is refused for the names an add would be refused for, with
// nothing committed.
func TestARenameIsTheClusterAdministratorsAndIsRefusedLikeAnAdd(t *testing.T) {
	src := catalogues(t, served{"gentian": {}, "partner": {}, "ours": {}})
	h := startCatalogues(t, src)
	alice := h.token(t, "gentian", "alice")
	if code, _ := h.do(t, "PUT", clusterCatalogues+"/partner", alice, urlBody(src.URL+"/partner")); code != http.StatusAccepted {
		t.Fatal("add partner")
	}
	if code, _ := h.do(t, "PUT", clusterTenant("demo", "/catalogues/ours"), alice, urlBody(src.URL+"/ours")); code != http.StatusAccepted {
		t.Fatal("add ours")
	}
	before := h.tip(t)
	for who, token := range map[string]string{
		"demo's administrator": h.token(t, "tenant-demo", "tom"),
		"a member":             h.token(t, "tenant-demo", "mia"),
		"an auditor":           h.token(t, "gentian", "audrey"),
	} {
		for _, path := range []string{clusterCatalogues + "/partner/actions/rename", clusterTenant("demo", "/catalogues/ours/actions/rename")} {
			if code, out := h.do(t, "POST", path, token, `{"name":"acme"}`); code != http.StatusForbidden {
				t.Errorf("%s, %s: %d %v, want 403", who, path, code, out)
			}
		}
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{clusterCatalogues + "/nowhere/actions/rename", `{"name":"acme"}`, http.StatusNotFound},
		{clusterCatalogues + "/ours/actions/rename", `{"name":"acme"}`, http.StatusNotFound}, // demo's, not the cluster's
		{clusterCatalogues + "/partner/actions/rename", `{"name":"gentian"}`, http.StatusConflict},
		{clusterCatalogues + "/partner/actions/rename", `{"name":"ours"}`, http.StatusConflict}, // demo has it
		{clusterCatalogues + "/partner/actions/rename", `{"name":"Acme"}`, http.StatusBadRequest},
		{clusterCatalogues + "/partner/actions/rename", `{"to":"acme"}`, http.StatusBadRequest},
		{clusterCatalogues + "/partner/actions/rename", ``, http.StatusBadRequest},
		{clusterTenant("demo", "/catalogues/partner/actions/rename"), `{"name":"acme"}`, http.StatusNotFound}, // the cluster's, not demo's
		{clusterTenant("demo", "/catalogues/ours/actions/rename"), `{"name":"gentian"}`, http.StatusConflict},
		{clusterTenant("solo", "/catalogues/ours/actions/rename"), `{"name":"acme"}`, http.StatusNotFound},
		{clusterTenant("nobody", "/catalogues/ours/actions/rename"), `{"name":"acme"}`, http.StatusNotFound},
	} {
		if code, out := h.do(t, "POST", c.path, alice, c.body); code != c.want {
			t.Errorf("POST %s %s: %d %v, want %d", c.path, c.body, code, out, c.want)
		}
	}
	// The same name again is not a change.
	if code, out := h.do(t, "POST", clusterCatalogues+"/partner/actions/rename", alice, `{"name":"partner"}`); code != http.StatusOK || out["status"] != "unchanged" {
		t.Errorf("rename to itself: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused rename moved the repository")
	}
}
