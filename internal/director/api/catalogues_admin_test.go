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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// Catalogues added by administrators.
//
// One web server plays every catalogue, each under its own path: the first
// path segment is the catalogue, and what it serves is that catalogue's
// profiles/<name>.yaml and an index.yaml listing them. The people are the
// fixture's: alice administers the cluster (and, through operated_by, tenant
// demo but not solo), tom administers demo, tina administers solo, mia is a
// member of demo.

// served is what each catalogue serves: catalogue -> profile name -> body.
type served map[string]map[string]string

func profileNamed(name, note string) string {
	return "apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: " + name +
		"\nspec:\n  classes: [app]\n  launch: tile\n  trustTier: experimental\n  version: \"1.0.0\"\n" +
		"  package:\n    chart:\n      repository: oci://registry.example.com/" + note + "\n      name: " + name +
		"\n      version: \"1.0.0\"\n"
}

func catalogues(t *testing.T, content served) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		profiles, ok := content[parts[0]]
		if !ok || len(parts) != 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if parts[1] == "index.yaml" {
			index := "entries:\n"
			for name, body := range profiles {
				index += "  - name: " + name + "\n    version: \"1.0.0\"\n    edition: pe\n    digest: " + sha(body) + "\n"
			}
			_, _ = w.Write([]byte(index))
			return
		}
		body, ok := profiles[strings.TrimSuffix(strings.TrimPrefix(parts[1], "profiles/"), ".yaml")]
		if !ok || !strings.HasPrefix(parts[1], "profiles/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startCatalogues is the harness with one catalogue of the whole cluster,
// "gentian", served by src under /gentian.
func startCatalogues(t *testing.T, src *httptest.Server, opts ...func(*api.Config)) *harness {
	t.Helper()
	return startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath: claimWith(gitops.CatalogueSource{Name: "gentian", URL: src.URL + "/gentian"}),
		})
	}, append([]func(*api.Config){withFetcher(src)}, opts...)...)
}

const clusterCatalogues = "/v1/clusters/" + dt.Cluster + "/catalogues"

func clusterTenant(tenant, rest string) string {
	return "/v1/clusters/" + dt.Cluster + "/tenants/" + tenant + rest
}

func urlBody(address string) string { return fmt.Sprintf(`{"url":%q}`, address) }

// names lists the catalogues of an answer as "name:scope:addedBy".
func names(out map[string]any) string {
	list, _ := out["catalogues"].([]any)
	parts := make([]string, 0, len(list))
	for _, raw := range list {
		c := raw.(map[string]any)
		parts = append(parts, fmt.Sprintf("%v:%v:%v", c["name"], c["scope"], c["addedBy"]))
	}
	return strings.Join(parts, " ")
}

func delegate(t *testing.T, h *harness, tenant string) {
	t.Helper()
	if code, out := h.do(t, "PUT", clusterTenant(tenant, "/catalogue-delegation"), h.token(t, "gentian", "alice"), ""); code != http.StatusAccepted {
		t.Fatalf("delegate %s: %d %v", tenant, code, out)
	}
}

// Every route asks one relation of one object, and that is what the commit
// records. The cluster's routes ask the cluster; a tenant's ask the tenant.
func TestEachCatalogueRouteAsksItsRelationOfItsObject(t *testing.T) {
	src := catalogues(t, served{"gentian": {}, "acme": {}, "ours": {}})
	h := startCatalogues(t, src)
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	cluster := "cluster:" + dt.Cluster

	for _, c := range []struct {
		method, path, token, body, asks string
		want                            int
	}{
		{"GET", clusterCatalogues, alice, "", "user:alice can_audit " + cluster, http.StatusOK},
		{"PUT", clusterCatalogues + "/extra", alice, urlBody(src.URL + "/acme"), "user:alice can_configure " + cluster, http.StatusAccepted},
		{"DELETE", clusterCatalogues + "/extra", alice, "", "user:alice can_configure " + cluster, http.StatusAccepted},
		{"PUT", clusterTenant("demo", "/catalogues/acme"), alice, urlBody(src.URL + "/acme"), "user:alice can_configure " + cluster, http.StatusAccepted},
		{"DELETE", clusterTenant("demo", "/catalogues/acme"), alice, "", "user:alice can_configure " + cluster, http.StatusAccepted},
		{"PUT", clusterTenant("demo", "/catalogue-delegation"), alice, "", "user:alice can_configure " + cluster, http.StatusAccepted},
		{"GET", "/v1/tenants/demo/catalogues", tom, "", "user:tom can_view tenant:demo", http.StatusOK},
		{"GET", "/v1/tenants/demo/catalogues/gentian/entries", tom, "", "user:tom can_view tenant:demo", http.StatusOK},
		{"PUT", "/v1/tenants/demo/catalogues/ours", tom, urlBody(src.URL + "/ours"), "user:tom can_install_app tenant:demo", http.StatusAccepted},
		{"DELETE", "/v1/tenants/demo/catalogues/ours", tom, "", "user:tom can_install_app tenant:demo", http.StatusAccepted},
		{"DELETE", clusterTenant("demo", "/catalogue-delegation"), alice, "", "user:alice can_configure " + cluster, http.StatusAccepted},
	} {
		h.asked.reset()
		code, out := h.do(t, c.method, c.path, c.token, c.body)
		if code != c.want {
			t.Fatalf("%s %s: %d %v, want %d", c.method, c.path, code, out, c.want)
		}
		if asked := h.asked.questions(); len(asked) != 1 || asked[0] != c.asks {
			t.Errorf("%s %s asked %v, want only %q", c.method, c.path, asked, c.asks)
		}
		if c.method == "GET" {
			continue
		}
		// A write is a commit by the person, recording the decision.
		trailer := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%an|%(trailers:key=Gentian-Authz,valueonly)", "main")
		who := "Alice|"
		if c.token == tom {
			who = "Tom|"
		}
		if !strings.Contains(trailer, who) || !strings.Contains(trailer, c.asks+" allowed") {
			t.Errorf("%s %s: commit = %q", c.method, c.path, trailer)
		}
	}
}

// Nobody but the cluster's administrator reaches the cluster's routes: not a
// tenant's administrator, not for their own tenant, and least of all the
// switch that says whether they may add catalogues themselves.
func TestOnlyTheClustersAdministratorChangesTheClustersCatalogues(t *testing.T) {
	src := catalogues(t, served{"gentian": {}, "acme": {}})
	h := startCatalogues(t, src)
	before := h.tip(t)
	for who, token := range map[string]string{
		"demo's administrator": h.token(t, "tenant-demo", "tom"),
		"solo's administrator": h.token(t, "tenant-solo", "tina"),
		"a member":             h.token(t, "tenant-demo", "mia"),
		"an auditor":           h.token(t, "gentian", "audrey"),
	} {
		for _, c := range []struct{ method, path, body string }{
			{"PUT", clusterCatalogues + "/acme", urlBody(src.URL + "/acme")},
			{"DELETE", clusterCatalogues + "/gentian", ""},
			{"PUT", clusterTenant("demo", "/catalogues/acme"), urlBody(src.URL + "/acme")},
			{"DELETE", clusterTenant("demo", "/catalogues/acme"), ""},
			{"PUT", clusterTenant("demo", "/catalogue-delegation"), ""},
			{"DELETE", clusterTenant("demo", "/catalogue-delegation"), ""},
		} {
			if code, out := h.do(t, c.method, c.path, token, c.body); code != http.StatusForbidden {
				t.Errorf("%s, %s %s: %d %v, want 403", who, c.method, c.path, code, out)
			}
		}
	}
	// There is no delegation route under a tenant at all.
	tom := h.token(t, "tenant-demo", "tom")
	for _, method := range []string{"PUT", "DELETE", "POST", "PATCH"} {
		if code, _ := h.do(t, method, "/v1/tenants/demo/catalogue-delegation", tom, `{"delegated":true}`); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/tenants/demo/catalogue-delegation answered %d", method, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
}

// A tenant's administrator adds a catalogue only where the cluster's
// administrator delegated that to the tenant. Not delegated, the request is
// refused before the address is so much as looked up; delegated, it is a
// commit that says the tenant added it.
func TestATenantAddsACatalogueOnlyWhenDelegated(t *testing.T) {
	src := catalogues(t, served{"gentian": {}, "ours": {}})
	var vetted atomic.Int32
	h := startCatalogues(t, src, func(cfg *api.Config) {
		cfg.Catalogue.Vet = func(context.Context, string) error { vetted.Add(1); return nil }
	})
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)

	code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/ours", tom, urlBody(src.URL+"/ours"))
	if code != http.StatusForbidden || !strings.Contains(fmt.Sprint(out["error"]), "not delegated") {
		t.Fatalf("not delegated: %d %v, want 403 saying so", code, out)
	}
	if vetted.Load() != 0 {
		t.Fatal("the address of a refused request was looked up")
	}
	if code, out := h.do(t, "DELETE", "/v1/tenants/demo/catalogues/ours", tom, ""); code != http.StatusForbidden {
		t.Fatalf("removing while not delegated: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
	// A member may not, delegated or not: the relation is asked first.
	delegate(t, h, "demo")
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/catalogues/ours", h.token(t, "tenant-demo", "mia"), urlBody(src.URL+"/ours")); code != http.StatusForbidden {
		t.Fatalf("a member added a catalogue: %d", code)
	}
	// Nor another tenant's administrator.
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/catalogues/ours", h.token(t, "tenant-solo", "tina"), urlBody(src.URL+"/ours")); code != http.StatusForbidden {
		t.Fatalf("another tenant's administrator added a catalogue: %d", code)
	}

	code, out = h.do(t, "PUT", "/v1/tenants/demo/catalogues/ours", tom, urlBody(src.URL+"/ours/"))
	if code != http.StatusAccepted || out["scope"] != "tenant" || out["tenant"] != "demo" {
		t.Fatalf("delegated: %d %v", code, out)
	}
	manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo"))
	for _, want := range []string{
		"  catalogue:\n    delegated: true\n    sources:\n      - name: ours\n        url: " + src.URL + "/ours\n        addedBy: tenant",
		"  apps:\n  - profile: nextcloud", // and what was there is as it was
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("the manifest is missing %q:\n%s", want, manifest)
		}
	}
	// The same again changes nothing.
	tip := h.tip(t)
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/ours", tom, urlBody(src.URL+"/ours")); code != http.StatusOK || out["status"] != "unchanged" || h.tip(t) != tip {
		t.Fatalf("the same catalogue again: %d %v", code, out)
	}
	// Delegation is per tenant: solo's administrator is still refused.
	if code, _ := h.do(t, "PUT", "/v1/tenants/solo/catalogues/ours", h.token(t, "tenant-solo", "tina"), urlBody(src.URL+"/ours")); code != http.StatusForbidden {
		t.Fatalf("delegating demo delegated solo: %d", code)
	}
	// Taken away again, what the tenant added stays and the tenant adds no more.
	if code, _ := h.do(t, "DELETE", clusterTenant("demo", "/catalogue-delegation"), h.token(t, "gentian", "alice"), ""); code != http.StatusAccepted {
		t.Fatalf("undelegate: %d", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/catalogues/more", tom, urlBody(src.URL+"/ours")); code != http.StatusForbidden {
		t.Fatalf("added after delegation was taken away: %d", code)
	}
	if _, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", tom, ""); names(out) != "gentian:cluster:cluster ours:tenant:tenant" || out["delegated"] != false {
		t.Fatalf("after undelegating: %v", out)
	}
}

// What the cluster's administrator decided is not a tenant administrator's
// to change: not the entries the cluster added for the tenant, and not the
// delegation, by any body the tenant's own routes accept.
func TestATenantCannotTouchWhatTheClusterAdded(t *testing.T) {
	src := catalogues(t, served{"gentian": {}, "partner": {}, "ours": {}})
	h := startCatalogues(t, src)
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")

	if code, out := h.do(t, "PUT", clusterTenant("demo", "/catalogues/partner"), alice, urlBody(src.URL+"/partner")); code != http.StatusAccepted {
		t.Fatalf("the cluster adds one for demo: %d %v", code, out)
	}
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(manifest, "addedBy: cluster") || strings.Contains(manifest, "delegated") {
		t.Fatalf("manifest:\n%s", manifest)
	}
	delegate(t, h, "demo")
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/ours", tom, urlBody(src.URL+"/ours")); code != http.StatusAccepted {
		t.Fatalf("the tenant adds its own: %d %v", code, out)
	}
	before := h.tip(t)

	// Not removed by the tenant, although it is delegated.
	code, out := h.do(t, "DELETE", "/v1/tenants/demo/catalogues/partner", tom, "")
	if code != http.StatusForbidden || !strings.Contains(fmt.Sprint(out["error"]), "cluster's administrator") {
		t.Fatalf("the tenant removed what the cluster added: %d %v", code, out)
	}
	// Not replaced either: the name is taken.
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/catalogues/partner", tom, urlBody(src.URL+"/ours")); code != http.StatusConflict {
		t.Fatalf("the tenant replaced what the cluster added: %d", code)
	}
	// And no body says who added an entry or whether the tenant is delegated.
	for _, body := range []string{
		fmt.Sprintf(`{"url":%q,"addedBy":"cluster"}`, src.URL+"/ours"),
		fmt.Sprintf(`{"url":%q,"delegated":true}`, src.URL+"/ours"),
		fmt.Sprintf(`{"url":%q,"scope":"cluster"}`, src.URL+"/ours"),
		fmt.Sprintf(`{"url":%q,"tenant":"solo"}`, src.URL+"/ours"),
	} {
		if code, _ := h.do(t, "PUT", "/v1/tenants/demo/catalogues/third", tom, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
	// The tenant's other writes leave the catalogue section as it is: an
	// install, an uninstall and a selection of add-ons edit the same file.
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, `{"defaultGrant":true}`); code != http.StatusAccepted {
		t.Fatalf("install: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom, `{"addons":["calendar"]}`); code != http.StatusAccepted {
		t.Fatalf("addons: %d %v", code, out)
	}
	if code, out := h.do(t, "DELETE", "/v1/tenants/demo/apps/nextcloud", tom, ""); code != http.StatusAccepted {
		t.Fatalf("uninstall: %d %v", code, out)
	}
	want := "  catalogue:\n    delegated: true\n    sources:\n      - name: partner\n        url: " + src.URL +
		"/partner\n        addedBy: cluster\n      - name: ours\n        url: " + src.URL + "/ours\n        addedBy: tenant\n"
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(manifest+"\n", want) {
		t.Fatalf("the catalogue section changed under other writes:\n%s", manifest)
	}
	// What the tenant added is the tenant's to remove; the cluster's
	// administrator removes either.
	if code, _ := h.do(t, "DELETE", "/v1/tenants/demo/catalogues/ours", tom, ""); code != http.StatusAccepted {
		t.Fatalf("the tenant removes its own: %d", code)
	}
	if code, _ := h.do(t, "DELETE", "/v1/tenants/demo/catalogues/ours", tom, ""); code != http.StatusNotFound {
		t.Fatalf("removing what is not there: %d", code)
	}
	if code, _ := h.do(t, "DELETE", clusterTenant("demo", "/catalogues/partner"), alice, ""); code != http.StatusAccepted {
		t.Fatalf("the cluster removes its own: %d", code)
	}
}

// A tenant sees the cluster's catalogues and its own. Another tenant's are
// not listed, cannot be read, and are refused in an install with the words an
// unknown catalogue is refused in.
func TestATenantSeesTheClustersCataloguesAndItsOwnAndNoOtherTenants(t *testing.T) {
	ours := profileNamed("demo-notes", "demo")
	src := catalogues(t, served{"gentian": {"element": elementProfile}, "ours": {"demo-notes": ours}})
	h := startCatalogues(t, src)
	alice, tom, tina := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-solo", "tina")
	delegate(t, h, "demo")
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/ours", tom, urlBody(src.URL+"/ours")); code != http.StatusAccepted {
		t.Fatalf("add: %d %v", code, out)
	}

	code, out := h.do(t, "GET", "/v1/tenants/demo/catalogues", tom, "")
	if code != http.StatusOK || names(out) != "gentian:cluster:cluster ours:tenant:tenant" || out["delegated"] != true {
		t.Fatalf("demo sees %d %v", code, out)
	}
	code, out = h.do(t, "GET", "/v1/tenants/solo/catalogues", tina, "")
	if code != http.StatusOK || names(out) != "gentian:cluster:cluster" || out["delegated"] != false {
		t.Fatalf("solo sees %d %v", code, out)
	}
	if strings.Contains(fmt.Sprint(out), "/ours") {
		t.Fatalf("solo's list carries demo's address: %v", out)
	}
	// Its entries, to its own tenant.
	code, out = h.do(t, "GET", "/v1/tenants/demo/catalogues/ours/entries", tom, "")
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(out["entries"]), "ours/demo-notes") || out["scope"] != "tenant" {
		t.Fatalf("demo's own entries: %d %v", code, out)
	}
	// To another tenant the catalogue does not exist.
	unknownCode, unknown := h.do(t, "GET", "/v1/tenants/solo/catalogues/nothing/entries", tina, "")
	code, out = h.do(t, "GET", "/v1/tenants/solo/catalogues/ours/entries", tina, "")
	if code != http.StatusNotFound || code != unknownCode || out["error"] != unknown["error"] {
		t.Fatalf("solo reading demo's catalogue: %d %v, an unknown one: %d %v", code, out, unknownCode, unknown)
	}
	before := h.tip(t)
	install := fmt.Sprintf(`{"coordinate":"ours/demo-notes","digest":%q}`, sha(ours))
	_, unknown = h.do(t, "POST", "/v1/tenants/solo/apps/demo-notes", tina,
		fmt.Sprintf(`{"coordinate":"nothing/demo-notes","digest":%q}`, sha(ours)))
	code, out = h.do(t, "POST", "/v1/tenants/solo/apps/demo-notes", tina, install)
	if code != http.StatusUnprocessableEntity ||
		strings.ReplaceAll(fmt.Sprint(out["error"]), `"ours"`, `"nothing"`) != fmt.Sprint(unknown["error"]) {
		t.Fatalf("solo installing from demo's catalogue: %d %v\nan unknown one: %v", code, out["error"], unknown["error"])
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}
	// A tenant's administrator does not read the cluster's list.
	if code, _ := h.do(t, "GET", clusterCatalogues, tom, ""); code != http.StatusForbidden {
		t.Fatalf("a tenant's administrator read the cluster's catalogues: %d", code)
	}
	// The cluster's administrator sees all of it, with who may add.
	code, out = h.do(t, "GET", clusterCatalogues, alice, "")
	if code != http.StatusOK || names(out) != "gentian:cluster:cluster" {
		t.Fatalf("the cluster's list: %d %v", code, out)
	}
	perTenant := map[string]string{}
	for _, raw := range out["tenants"].([]any) {
		entry := raw.(map[string]any)
		perTenant[entry["tenant"].(string)] = fmt.Sprintf("%v %s", entry["delegated"], names(entry))
	}
	if perTenant["demo"] != "true ours:tenant:tenant" || perTenant["solo"] != "false " || perTenant["other"] != "false " {
		t.Fatalf("per tenant: %v", perTenant)
	}
}

// A name means one catalogue to a tenant. A tenant's catalogue does not take
// the name of one of the cluster's or of another of its own, and the cluster
// does not take a name a tenant already uses; two tenants may each use the
// same name, and each gets its own.
func TestACatalogueNameMeansOneCatalogueToATenant(t *testing.T) {
	demoApp, soloApp := profileNamed("demo-app", "demo"), profileNamed("solo-app", "solo")
	src := catalogues(t, served{"gentian": {}, "demo-own": {"demo-app": demoApp}, "solo-own": {"solo-app": soloApp}, "other": {}})
	h := startCatalogues(t, src)
	alice, tom, tina := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-solo", "tina")
	delegate(t, h, "demo")
	delegate(t, h, "solo")

	code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/gentian", tom, urlBody(src.URL+"/demo-own"))
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "whole cluster") {
		t.Fatalf("a tenant took a cluster catalogue's name: %d %v", code, out)
	}
	// The same name in two tenants, at two addresses.
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/own", tom, urlBody(src.URL+"/demo-own")); code != http.StatusAccepted {
		t.Fatalf("demo: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", "/v1/tenants/solo/catalogues/own", tina, urlBody(src.URL+"/solo-own")); code != http.StatusAccepted {
		t.Fatalf("solo: %d %v", code, out)
	}
	if code, _ := h.do(t, "PUT", "/v1/tenants/demo/catalogues/own", tom, urlBody(src.URL+"/other")); code != http.StatusConflict {
		t.Fatalf("a second catalogue of the same name in one tenant: %d", code)
	}
	// The cluster cannot take it now, and is told whose it is.
	code, out = h.do(t, "PUT", clusterCatalogues+"/own", alice, urlBody(src.URL+"/other"))
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "demo, solo") {
		t.Fatalf("the cluster took a tenant catalogue's name: %d %v", code, out)
	}
	if code, _ := h.do(t, "PUT", clusterCatalogues+"/gentian", alice, urlBody(src.URL+"/other")); code != http.StatusConflict {
		t.Fatalf("a second cluster catalogue of the same name: %d", code)
	}

	// Each tenant's "own" is its own: the same coordinate prefix fetches
	// from different addresses, and each profile is recorded as its tenant's.
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/demo-app", tom, fmt.Sprintf(`{"coordinate":"own/demo-app","digest":%q}`, sha(demoApp))); code != http.StatusAccepted {
		t.Fatalf("demo installs from its own: %d %v", code, out)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/solo/apps/solo-app", tina, fmt.Sprintf(`{"coordinate":"own/solo-app","digest":%q}`, sha(soloApp))); code != http.StatusAccepted {
		t.Fatalf("solo installs from its own: %d %v", code, out)
	}
	// What demo's serves is not in solo's, although both are called "own".
	if code, _ := h.do(t, "POST", "/v1/tenants/solo/apps/demo-app", tina, fmt.Sprintf(`{"coordinate":"own/demo-app","digest":%q}`, sha(demoApp))); code != http.StatusNotFound {
		t.Fatalf("solo fetched from demo's catalogue through a shared name: %d", code)
	}
	for profile, origin := range map[string]string{"demo-app": "tenant/demo/own", "solo-app": "tenant/solo/own"} {
		bundle := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BundleFile(profile)))
		if !strings.Contains(bundle, profilebundle.OriginAnnotation+": "+origin+"\n") && !strings.HasSuffix(bundle, profilebundle.OriginAnnotation+": "+origin) {
			t.Errorf("%s is not recorded as %s:\n%s", profile, origin, bundle)
		}
	}
	// The pin says the catalogue by the name the tenant knows it by.
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(manifest, "  - profile: demo-app\n    digest: "+sha(demoApp)+"\n    catalogue: own\n") {
		t.Fatalf("demo's pin:\n%s", manifest)
	}
}

// ComponentProfiles are cluster-scoped: one name, one profile. A tenant's
// catalogue does not take a name another origin has, and nothing takes a name
// a tenant's catalogue has. The refusal says the name is taken and what to
// publish instead; the same catalogue installing its own entry again, or a
// new build of it, is not a clash.
func TestAProfileNameTakenByAnotherOriginIsRefused(t *testing.T) {
	theirs := profileNamed("element", "demo-fork")
	notes, notes2 := profileNamed("notes-app", "demo"), profileNamed("notes-app", "demo-v2")
	soloNotes := profileNamed("notes-app", "solo")
	desktop, wiki := profileNamed("desktop", "demo"), profileNamed("wiki", "demo")
	src := catalogues(t, served{
		"gentian": {"element": elementProfile, "notes-app": profileNamed("notes-app", "gentian"), "desktop": desktop},
		"ours":    {"element": theirs, "notes-app": notes, "desktop": desktop, "wiki": wiki},
		"ours-v2": {"notes-app": notes2},
		"solos":   {"notes-app": soloNotes},
	})
	h := startCatalogues(t, src)
	alice, tom, tina := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-solo", "tina")
	for tenant, path := range map[string]string{"demo": "ours", "solo": "solos"} {
		if code, out := h.do(t, "PUT", clusterTenant(tenant, "/catalogues/own"), alice, urlBody(src.URL+"/"+path)); code != http.StatusAccepted {
			t.Fatalf("add for %s: %d %v", tenant, code, out)
		}
	}
	install := func(token, tenant, coordinate, body string) (int, map[string]any) {
		_, app, _ := strings.Cut(coordinate, "/")
		return h.do(t, "POST", "/v1/tenants/"+tenant+"/apps/"+app, token, fmt.Sprintf(`{"coordinate":%q,"digest":%q}`, coordinate, sha(body)))
	}

	// The cluster's catalogue first, then a tenant's under the same name.
	if code, out := install(tina, "solo", "gentian/element", elementProfile); code != http.StatusAccepted {
		t.Fatalf("from the cluster's catalogue: %d %v", code, out)
	}
	before := h.tip(t)
	code, out := install(tom, "demo", "own/element", theirs)
	said := fmt.Sprint(out["error"])
	if code != http.StatusConflict || !strings.Contains(said, "already taken") || !strings.Contains(said, "demo-element") {
		t.Fatalf("a tenant's profile took a name the cluster's catalogue has: %d %v", code, out)
	}
	// A profile the platform itself ships, from either kind of catalogue.
	code, out = install(tom, "demo", "own/desktop", desktop)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "platform ships") || !strings.Contains(fmt.Sprint(out["error"]), "demo-desktop") {
		t.Fatalf("a tenant's profile took a platform component's name: %d %v", code, out)
	}
	if code, out = install(tina, "solo", "gentian/desktop", desktop); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "platform ships") {
		t.Fatalf("a catalogue's profile took a platform component's name: %d %v", code, out)
	}
	// A profile that was on the cluster before origins were recorded.
	// (wiki is in every fixture's catalogue directory, with no bundle.)
	if code, out = install(tom, "demo", "own/wiki", wiki); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "demo-wiki") {
		t.Fatalf("a tenant's profile took the name of one with no recorded origin: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}

	// A tenant's catalogue first, then the other direction.
	if code, out := install(tom, "demo", "own/notes-app", notes); code != http.StatusAccepted {
		t.Fatalf("a tenant's own profile: %d %v", code, out)
	}
	before = h.tip(t)
	// Another tenant's catalogue: refused, and not told whose the name is.
	code, out = install(tina, "solo", "own/notes-app", soloNotes)
	said = fmt.Sprint(out["error"])
	if code != http.StatusConflict || !strings.Contains(said, "solo-notes-app") || strings.Contains(said, "demo") {
		t.Fatalf("another tenant's profile took the name: %d %v", code, out)
	}
	// The cluster's catalogue: refused, saying it is the tenant's to rename.
	code, out = install(tina, "solo", "gentian/notes-app", profileNamed("notes-app", "gentian"))
	said = fmt.Sprint(out["error"])
	if code != http.StatusConflict || !strings.Contains(said, "tenant's own catalogue") || !strings.Contains(said, "has to rename") || strings.Contains(said, "demo") {
		t.Fatalf("the cluster's profile took a tenant's name: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}

	// The same build of the same origin again is fine, and commits nothing
	// for the profile.
	if code, out := install(tom, "demo", "own/notes-app", notes); code != http.StatusOK || out["status"] != "already_installed" {
		t.Fatalf("the same build again: %d %v", code, out)
	}
	// And a new build of the same origin moves the pin: an update.
	if code, _ := h.do(t, "DELETE", clusterTenant("demo", "/catalogues/own"), alice, ""); code != http.StatusAccepted {
		t.Fatal("remove")
	}
	if code, _ := h.do(t, "PUT", clusterTenant("demo", "/catalogues/own"), alice, urlBody(src.URL+"/ours-v2")); code != http.StatusAccepted {
		t.Fatal("add again")
	}
	if code, out := install(tom, "demo", "own/notes-app", notes2); code != http.StatusAccepted || out["status"] != "updated" {
		t.Fatalf("a new build of the same origin: %d %v", code, out)
	}
}

// A profile from a tenant's own catalogue is installable in that tenant and
// in no other -- by coordinate, which the clash rule above covers, and by
// name alone, which is this. The answer to another tenant is the one for a
// profile that is not on the cluster.
func TestATenantsOwnProfileIsNotInstalledInAnotherTenant(t *testing.T) {
	app := profileNamed("demo-app", "demo")
	addon := "apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: demo-wiki-search\nspec:\n  classes: [app]\n" +
		"  launch: none\n  trustTier: experimental\n  version: \"1.0.0\"\n  package:\n    addon:\n      of: demo-app\n"
	src := catalogues(t, served{"gentian": {}, "ours": {"demo-app": app, "demo-wiki-search": addon}})
	h := startCatalogues(t, src)
	alice, tom, tina := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-solo", "tina")
	if code, out := h.do(t, "PUT", clusterTenant("demo", "/catalogues/ours"), alice, urlBody(src.URL+"/ours")); code != http.StatusAccepted {
		t.Fatalf("add: %d %v", code, out)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/demo-app", tom, fmt.Sprintf(`{"coordinate":"ours/demo-app","digest":%q}`, sha(app))); code != http.StatusAccepted {
		t.Fatalf("demo installs its own: %d %v", code, out)
	}
	// Into the app it pinned: an add-on is pinned only inside a pinned app.
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/demo-app/addons", tom,
		fmt.Sprintf(`{"addons":[{"coordinate":"ours/demo-wiki-search","digest":%q}]}`, sha(addon))); code != http.StatusAccepted {
		t.Fatalf("demo switches its own add-on on: %d %v", code, out)
	}
	before := h.tip(t)

	// By name alone, in the tenant it belongs to: there, and usable.
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/demo-app", tom, ""); code != http.StatusOK {
		t.Fatalf("demo, by name: %d %v", code, out)
	}
	// In another tenant: refused as a profile that is not there.
	_, absent := h.do(t, "POST", "/v1/tenants/solo/apps/no-such-app", tina, "")
	code, out := h.do(t, "POST", "/v1/tenants/solo/apps/demo-app", tina, "")
	if code != http.StatusUnprocessableEntity ||
		strings.ReplaceAll(fmt.Sprint(out["error"]), "demo-app", "no-such-app") != fmt.Sprint(absent["error"]) {
		t.Fatalf("solo, by name: %d %v\nan absent one: %v", code, out["error"], absent["error"])
	}
	// The add-on likewise, into an app solo does have.
	code, out = h.do(t, "PUT", "/v1/tenants/solo/apps/nextcloud/addons", tina, `{"addons":["demo-wiki-search"]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), "coordinate and digest") {
		t.Fatalf("solo switching demo's add-on on: %d %v", code, out)
	}
	// The cluster's administrator installing into solo is refused as well:
	// it is not a question of who asks.
	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/demo-app", alice, ""); code != http.StatusOK {
		t.Fatalf("alice in demo: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}
}

// Nothing puts profiles on a cluster ahead of an install any more. An app or
// an add-on named without a build is valid for a profile the cluster already
// holds, and otherwise refused with what to send instead -- never written to
// the manifest to activate nothing.
func TestANameWithNoBuildNeedsAProfileOnTheCluster(t *testing.T) {
	src := catalogues(t, served{"gentian": {}})
	h := startCatalogues(t, src)
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)

	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/pad", tom, "")
	said := fmt.Sprint(out["error"])
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("an app that is not on the cluster: %d %v", code, out)
	}
	for _, want := range []string{"no profile of that name is on this cluster", "coordinate and digest", `"<catalogue>/pad"`, "Nothing was installed", "catalogues: gentian"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q: %s", want, said)
		}
	}
	code, out = h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["calendar","nextcloud-talk"]}`)
	said = fmt.Sprint(out["error"])
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("an add-on that is not on the cluster: %d %v", code, out)
	}
	for _, want := range []string{"add-on nextcloud-talk", "activate nothing", "coordinate and digest", `"<catalogue>/nextcloud-talk"`, "Nothing was changed"} {
		if !strings.Contains(said, want) {
			t.Errorf("the add-on refusal does not say %q: %s", want, said)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
	// What is on the cluster installs by name, as before.
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom, ""); code != http.StatusAccepted {
		t.Fatalf("an app that is on the cluster: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/nextcloud/addons", tom, `{"addons":["calendar"]}`); code != http.StatusAccepted {
		t.Fatalf("an add-on that is on the cluster: %d %v", code, out)
	}
}

// An uninstalled app's profile stays in the cluster's catalogue directory:
// its data is still there, and a purge needs the profile to know what the
// data is. Nothing removes it when the last install goes.
func TestAnUninstalledAppsProfileStays(t *testing.T) {
	app := profileNamed("notes-app", "gentian")
	src := catalogues(t, served{"gentian": {"notes-app": app}})
	h := startCatalogues(t, src)
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes-app", tom, fmt.Sprintf(`{"coordinate":"gentian/notes-app","digest":%q}`, sha(app))); code != http.StatusAccepted {
		t.Fatalf("install: %d %v", code, out)
	}
	if code, out := h.do(t, "DELETE", "/v1/tenants/demo/apps/notes-app", tom, ""); code != http.StatusAccepted {
		t.Fatalf("uninstall: %d %v", code, out)
	}
	for _, file := range []string{"notes-app.yaml", gitops.BundleFile("notes-app")} {
		if got := dt.RemoteFile(t, h.remote, dt.CataloguePath(file)); got == "" {
			t.Errorf("%s is gone after the uninstall", file)
		}
	}
	k := dt.RemoteFile(t, h.remote, dt.CataloguePath("kustomization.yaml"))
	if !strings.Contains(k, "- notes-app.yaml") || !strings.Contains(k, "notes-app.bundle.yaml") {
		t.Fatalf("the kustomization no longer applies it:\n%s", k)
	}
	// And it installs again by name: the profile is on the cluster.
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes-app", tom, ""); code != http.StatusAccepted {
		t.Fatalf("install again by name: %d %v", code, out)
	}
}

// An address is checked whoever sends it, with the checks the director's own
// fetcher makes: the cluster's administrator's typo must not probe the
// cluster, and a delegated tenant's must not either.
func TestAnAddressThatReachesInsideIsRefusedOnEveryRoute(t *testing.T) {
	h := startSeeded(t, nil, nil, func(cfg *api.Config) { cfg.Catalogue = catalogue.NewFetcher() })
	alice, tom := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom")
	delegate(t, h, "demo")
	before := h.tip(t)
	for _, address := range []string{
		"http://catalogue.example.com/apps",
		"https://kubernetes.default.svc/api",
		"https://openbao.gentian-vault.svc.cluster.local:8200/v1/sys/health",
		"https://director/",
		"https://127.0.0.1/",
		"https://[::1]/",
		"https://10.43.0.1/",
		"https://172.16.3.4/",
		"https://192.168.1.1/",
		"https://100.64.0.1/",
		"https://169.254.169.254/latest/meta-data/",
		"https://[fd00:ec2::254]/",
		"https://[fe80::1]/",
		"https://0.0.0.0/",
		"https://224.0.0.1/",
		"https://user:pw@catalogue.example.com/",
		"https://catalogue.example.com:6443/",
		h.URL, // the director itself: plain http, on loopback
	} {
		for route, token := range map[string]string{
			clusterCatalogues + "/typo":               alice,
			clusterTenant("demo", "/catalogues/typo"): alice,
			"/v1/tenants/demo/catalogues/typo":        tom,
		} {
			code, out := h.do(t, "PUT", route, token, urlBody(address))
			if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), "not one a catalogue is fetched from") {
				t.Errorf("%s on %s: %d %v, want 422", address, route, code, out)
			}
		}
	}
	for _, body := range []string{``, `{}`, `{"url":""}`, `{"address":"https://catalogue.example.com"}`} {
		if code, _ := h.do(t, "PUT", clusterCatalogues+"/typo", alice, body); code != http.StatusBadRequest {
			t.Errorf("body %q: %d, want 400", body, code)
		}
	}
	if code, _ := h.do(t, "PUT", clusterCatalogues+"/Not_A_Name", alice, urlBody("https://catalogue.example.com")); code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
		t.Errorf("a name that is not a DNS label: %d", code)
	}
	if h.tip(t) != before {
		t.Fatal("a refused address moved the repository")
	}
}

// A catalogue added through the routes is there for the next request: the
// director reads what there is when it is asked, not once when it starts.
func TestACatalogueAddedIsThereForTheNextInstall(t *testing.T) {
	app := profileNamed("planner", "partner")
	src := catalogues(t, served{"gentian": {}, "partner": {"planner": app}})
	h := startCatalogues(t, src)
	alice, tom, tina := h.token(t, "gentian", "alice"), h.token(t, "tenant-demo", "tom"), h.token(t, "tenant-solo", "tina")
	install := fmt.Sprintf(`{"coordinate":"partner/planner","digest":%q}`, sha(app))

	if code, _ := h.do(t, "POST", "/v1/tenants/demo/apps/planner", tom, install); code != http.StatusUnprocessableEntity {
		t.Fatalf("before the catalogue exists: %d", code)
	}
	code, out := h.do(t, "PUT", clusterCatalogues+"/partner", alice, urlBody(src.URL+"/partner"))
	if code != http.StatusAccepted || out["scope"] != "cluster" {
		t.Fatalf("add: %d %v", code, out)
	}
	claim := dt.RemoteFile(t, h.remote, dt.ClaimPath)
	want := "    storeUrl: https://store.example.com\n    sources:\n      - name: gentian\n        url: " + src.URL +
		"/gentian\n      - name: partner\n        url: " + src.URL + "/partner"
	if !strings.Contains(claim, want) {
		t.Fatalf("the claim:\n%s", claim)
	}
	// Every tenant sees it, and installs from it, at once.
	if _, out := h.do(t, "GET", "/v1/tenants/solo/catalogues", tina, ""); names(out) != "gentian:cluster:cluster partner:cluster:cluster" {
		t.Fatalf("solo sees %v", out)
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/planner", tom, install); code != http.StatusAccepted {
		t.Fatalf("after the catalogue was added: %d %v", code, out)
	}
	bundle := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BundleFile("planner")))
	if !strings.Contains(bundle, profilebundle.OriginAnnotation+": cluster/partner") {
		t.Fatalf("the profile's origin:\n%s", bundle)
	}
	// Removed, it is gone for the next request; what was installed stays.
	if code, _ := h.do(t, "DELETE", clusterCatalogues+"/partner", alice, ""); code != http.StatusAccepted {
		t.Fatal("remove")
	}
	if code, _ := h.do(t, "POST", "/v1/tenants/solo/apps/planner", tina, install); code != http.StatusUnprocessableEntity {
		t.Fatalf("after the catalogue was removed: %d", code)
	}
	if code, _ := h.do(t, "DELETE", clusterCatalogues+"/partner", alice, ""); code != http.StatusNotFound {
		t.Fatal("removing what is not there")
	}
	if manifest := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(manifest, "profile: planner") {
		t.Fatalf("the install went with the catalogue:\n%s", manifest)
	}
	// It is on the cluster, and of the whole cluster's origin, so another
	// tenant installs it by name.
	if code, out := h.do(t, "POST", "/v1/tenants/solo/apps/planner", tina, ""); code != http.StatusAccepted {
		t.Fatalf("by name, in another tenant: %d %v", code, out)
	}
}

// The platform tenant takes no apps, so it takes no catalogues either.
func TestThePlatformTenantTakesNoCatalogues(t *testing.T) {
	src := catalogues(t, served{"gentian": {}, "x": {}})
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			"clusters/" + dt.Cluster + "/tenants/platform/tenant.yaml": "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: platform\n" +
				"spec:\n  displayName: Platform\n  isolation:\n    keycloakRealm: gentian\n  apps: []\n",
		})
	}, withFetcher(src))
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)
	if code, out := h.do(t, "PUT", clusterTenant("platform", "/catalogues/x"), alice, urlBody(src.URL+"/x")); code != http.StatusConflict {
		t.Fatalf("a catalogue for the platform tenant: %d %v", code, out)
	}
	if code, out := h.do(t, "PUT", clusterTenant("platform", "/catalogue-delegation"), alice, ""); code != http.StatusConflict {
		t.Fatalf("delegation for the platform tenant: %d %v", code, out)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request moved the repository")
	}
}
