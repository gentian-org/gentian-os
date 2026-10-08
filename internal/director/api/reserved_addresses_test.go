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
	"github.com/gentian-org/gentian-os/internal/hostnames"
)

// appAt is a profile with one entry at a label: what a catalogue serves, or
// what the cluster's catalogue directory holds.
func appAt(name, subDomain, tier string) string {
	entry := "      subDomain: " + subDomain + "\n"
	if subDomain == "" {
		entry = ""
	}
	return `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: ` + name + `
spec:
  classes: [app]
  launch: none
  trustTier: ` + tier + `
  version: "1.0.0"
  package:
    chart:
      repository: oci://example.invalid/` + name + `
      name: ` + name + `
      version: "1.0.0"
  expose:
    - name: web
      surface: gateway
      authMode: oidc
` + entry + `      backend: {service: ` + name + `, port: 8080}
`
}

func singleClaim(sources ...gitops.CatalogueSource) string {
	return claimWith(sources...) + "  tenancyMode: single\n"
}

// An app from a catalogue that would answer on an address name the platform
// keeps is refused when it is installed, with the reason, and nothing is
// committed -- whatever trust tier its profile states, and whatever it is
// called.
func TestAnAppAtAReservedAddressIsRefusedAtInstall(t *testing.T) {
	served := map[string]string{
		"notes":   appAt("notes", "notes", "certified"),
		"wiki":    appAt("wiki", "admin", "certified"),
		"board":   appAt("board", "desktop", "platform"),
		"status":  appAt("status", "console", "certified"),
		"console": appAt("console", "", "certified"),
		"app-0":   appAt("app-0", "login", "certified"),
		"app-1":   appAt("app-1", "llm", "certified"),
		"app-2":   appAt("app-2", "www", "certified"),
	}
	src := addonSource(t, served)
	h := startWithCatalogue(t, src, map[string]string{"main": src.URL})
	tom := h.token(t, "tenant-demo", "tom")
	install := func(name string) (int, map[string]any) {
		return h.do(t, "POST", "/v1/tenants/demo/apps/"+name, tom,
			fmt.Sprintf(`{"coordinate":"main/%s","digest":%q}`, name, sha(served[name])))
	}
	before := h.tip(t)
	for name, says := range map[string][]string{
		"wiki":    {"app wiki cannot be installed", `exposure "web"`, "admin.<the tenant's domain>", "administration console", "admin-console", "Nothing was committed or installed"},
		"board":   {"desktop.<the tenant's domain>", "the tenant's desktop"},
		"status":  {"console.<the tenant's domain>", "former address", "nothing may take it"},
		"console": {"console.<the tenant's domain>"},
		"app-0":   {"login.<the tenant's domain>", "sign-in page"},
	} {
		code, out := install(name)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: install = %d %v, want 422", name, code, out)
		}
		for _, want := range says {
			if !strings.Contains(fmt.Sprint(out["error"]), want) {
				t.Errorf("%s: the refusal does not say %q: %v", name, want, out["error"])
			}
		}
		if h.tip(t) != before {
			t.Fatalf("%s: a refused install moved the repository", name)
		}
	}
	// Every name on the list, so that one added there is refused here.
	for _, r := range hostnames.PlatformLabels() {
		served["app-3"] = appAt("app-3", r.Label, "certified")
		if code, out := install("app-3"); code != http.StatusUnprocessableEntity {
			t.Fatalf("an app at %s: install = %d %v, want 422", r.Label, code, out)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}
	// An address of its own, and the kernel's names on a cluster whose
	// tenants have domains of their own, are an app's to take.
	for _, name := range []string{"notes", "app-1", "app-2"} {
		if code, out := install(name); code != http.StatusAccepted {
			t.Fatalf("%s: install = %d %v", name, code, out)
		}
	}
}

// On a single-tenancy cluster the tenant's hosts are directly under the
// cluster's domain, and the kernel's own names are refused as well.
func TestAnAppAtAKernelAddressIsRefusedOnASingleTenancyCluster(t *testing.T) {
	served := map[string]string{
		"notes": appAt("notes", "notes", "certified"),
		"app-1": appAt("app-1", "llm", "certified"),
		"app-2": appAt("app-2", "www", "certified"),
		"mail":  appAt("mail", "", "certified"),
		"wiki":  appAt("wiki", "admin", "certified"),
	}
	src := addonSource(t, served)
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{dt.ClaimPath: singleClaim(gitops.CatalogueSource{Name: "main", URL: src.URL})})
	}, withFetcher(src))
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	for _, name := range []string{"app-1", "app-2", "mail", "wiki"} {
		code, out := h.do(t, "POST", "/v1/tenants/demo/apps/"+name, tom,
			fmt.Sprintf(`{"coordinate":"main/%s","digest":%q}`, name, sha(served[name])))
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: install = %d %v, want 422", name, code, out)
		}
		if name != "wiki" && !strings.Contains(fmt.Sprint(out["error"]), "tenancy mode is single") {
			t.Errorf("%s: the refusal does not say why here: %v", name, out["error"])
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused install moved the repository")
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes", tom,
		fmt.Sprintf(`{"coordinate":"main/notes","digest":%q}`, sha(served["notes"]))); code != http.StatusAccepted {
		t.Fatalf("notes: install = %d %v", code, out)
	}
}

// An install by name is of a profile the cluster already holds, and the
// director reads what it holds from the catalogue directory: the same
// refusal, before the tenant's manifest names the app.
func TestAnAppAlreadyOnTheClusterAtAReservedAddressIsRefusedByName(t *testing.T) {
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.CataloguePath("wiki.yaml"):   appAt("wiki", "admin", "platform"),
			dt.CataloguePath("notes.yaml"):  appAt("notes", "notes", "certified"),
			dt.CataloguePath("board.yaml"):  "not: [a profile\n",
			dt.CataloguePath("status.yaml"): appAt("somebody-else", "status", "certified"),
			// A profile called desktop in the directory, as a catalogue's
			// would be: with the origin the director records.
			dt.CataloguePath("deck.yaml"): appAt("deck", "desktop", "platform"),
			dt.CataloguePath(gitops.BundleFile("deck")): "apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: deck\n" +
				"  annotations:\n    gentianos.io/catalogue-origin: cluster/main\n",
		})
	})
	tom := h.token(t, "tenant-demo", "tom")
	before := h.tip(t)
	for name, says := range map[string]string{
		"wiki":   "admin.<the tenant's domain>",
		"deck":   "desktop.<the tenant's domain>",
		"board":  "could not be read",
		"status": "could not be read",
	} {
		code, out := h.do(t, "POST", "/v1/tenants/demo/apps/"+name, tom, "")
		if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), says) {
			t.Fatalf("%s: a bare install = %d %v, want 422 saying %q", name, code, out, says)
		}
		if h.tip(t) != before {
			t.Fatalf("%s: a refused install moved the repository", name)
		}
	}
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/notes", tom, ""); code != http.StatusAccepted {
		t.Fatalf("notes: a bare install = %d %v", code, out)
	}
}

// An add-on is under the same rule, by name and at a stated build. An entry
// its base serves has the base's host and is not asked.
func TestAnAddonAtAReservedAddressIsRefused(t *testing.T) {
	addonAt := func(name, subDomain, servedBy string) string {
		body := strings.Replace(appAt(name, subDomain, "certified"),
			"    chart:\n      repository: oci://example.invalid/"+name+"\n      name: "+name+"\n      version: \"1.0.0\"\n",
			"    addon:\n      of: element\n", 1)
		if servedBy != "" {
			body = strings.Replace(body, "backend: {service: "+name+", port: 8080}",
				"backend: {component: "+servedBy+", service: "+servedBy+", port: 8080}", 1)
		}
		return body
	}
	served := map[string]string{
		"element":      elementProfile,
		"element-talk": addonAt("element-talk", "store", ""),
		"element-deck": addonAt("element-deck", "admin", "element"),
	}
	src := addonSource(t, served)
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath:                      claimWith(gitops.CatalogueSource{Name: "main", URL: src.URL}),
			dt.CataloguePath("calendar.yaml"): addonAt("calendar", "sso", ""),
		})
	}, withFetcher(src))
	tom := h.token(t, "tenant-demo", "tom")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/element", tom,
		fmt.Sprintf(`{"coordinate":"main/element","digest":%q}`, sha(elementProfile))); code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	before := h.tip(t)
	for name, c := range map[string]struct{ entry, says string }{
		"at a stated build": {pinnedAddon("main/element-talk", sha(served["element-talk"])), "add-on element-talk cannot be installed"},
		"by name":           {`"calendar"`, "add-on calendar cannot be installed"},
	} {
		code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom, `{"addons":[`+c.entry+`]}`)
		if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), c.says) {
			t.Fatalf("%s: %d %v, want 422 saying %q", name, code, out, c.says)
		}
		if h.tip(t) != before {
			t.Fatalf("%s: a refused selection moved the repository", name)
		}
	}
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/apps/element/addons", tom,
		`{"addons":[`+pinnedAddon("main/element-deck", sha(served["element-deck"]))+`]}`); code != http.StatusAccepted {
		t.Fatalf("an add-on whose entry its base serves = %d %v", code, out)
	}
}

// An import gives a tenant every app its bundle lists, so it is asked the
// same: a definition that would take a reserved address name is named up
// front, fetched or already here, and nothing is changed.
func TestAnImportIsRefusedWhenAnAppWouldTakeAReservedAddress(t *testing.T) {
	served := map[string]string{"element": appAt("element", "desktop", "platform")}
	src := addonSource(t, served)
	op := &importingOperator{declared: true,
		manifest: `{"schemaVersion":2,"tenant":"imported","export":"nightly","createdAt":"2026-10-01T03:00:00Z","tenantSpec":{"displayName":"Imported Ltd","apps":[` +
			`{"profile":"element","digest":"` + sha(served["element"]) + `","catalogue":"main"},` +
			`{"profile":"wiki"},{"profile":"notes"}]},"apps":[]}`}
	h := startSeeded(t, op, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath:                  claimWith(gitops.CatalogueSource{Name: "main", URL: src.URL}),
			dt.CataloguePath("wiki.yaml"): appAt("wiki", "login", "certified"),
		})
	}, withFetcher(src))
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)
	code, body := h.do(t, "POST", "/v1/clusters/"+dt.Cluster+"/tenants/import", alice, goodImport+`}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("import = %d %v", code, body)
	}
	said := fmt.Sprint(body)
	for _, want := range []string{"app element (", "desktop.<the tenant's domain>", "app wiki (", "login.<the tenant's domain>"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, "app notes (") {
		t.Errorf("an app at an address of its own is named:\n%s", said)
	}
	if h.tip(t) != before {
		t.Error("a refused import still committed something")
	}
}
