/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// shopBundle is a profile and what travels with it: one file, as a catalogue
// publishes it.
func shopBundle(extra ...string) string {
	profile := "apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: shop\n" +
		"spec:\n  classes: [app]\n  launch: tile\n  trustTier: experimental\n  version: \"1.0.0\"\n" +
		"  package:\n    composition: app-shop\n    chart:\n      repository: oci://registry.example.com/shop\n      name: shop\n      version: \"1.0.0\"\n"
	composition := "apiVersion: apiextensions.crossplane.io/v1\nkind: Composition\nmetadata:\n  name: app-shop\n" +
		"  labels:\n    gentianos.io/profile-name: shop\nspec:\n  compositeTypeRef:\n    apiVersion: gentianos.io/v1alpha1\n    kind: XApp\n" +
		"  mode: Pipeline\n  pipeline:\n    - step: render\n      functionRef:\n        name: function-go-templating\n"
	page := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shop.portal-bridge-sso\n" +
		"  labels:\n    gentianos.io/profile-name: shop\n    gentianos.io/asset: portal-bridge-sso\ndata:\n  sso.html: \"<html></html>\"\n"
	return strings.Join(append([]string{profile, composition, page}, extra...), "---\n")
}

func catalogueHas(t *testing.T, h *harness, file string) bool {
	t.Helper()
	listing := dt.Git(t, "", "--git-dir", h.remote, "ls-tree", "-r", "--name-only", "main")
	return strings.Contains(listing, dt.CataloguePath(file)+"\n") || strings.HasSuffix(listing, dt.CataloguePath(file))
}

// One profile, one file, one fingerprint: a bundle that brings companions is
// committed exactly as a profile alone is -- the file whole, once in the
// kustomization, and its bytes whole in the carrier -- so Argo CD applies the
// profile and its companions together and the operator can check each.
func TestABundleWithCompanionsIsCommittedWhole(t *testing.T) {
	bundle := shopBundle()
	src := catalogues(t, served{"gentian": {"shop": bundle}})
	h := startCatalogues(t, src)
	tom := h.token(t, "tenant-demo", "tom")

	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/shop", tom, fmt.Sprintf(`{"coordinate":"gentian/shop","digest":%q}`, sha(bundle)))
	if code != http.StatusAccepted {
		t.Fatalf("install = %d %v", code, out)
	}
	got := dt.RemoteFile(t, h.remote, dt.CataloguePath("shop.yaml"))
	if sha(strings.TrimRight(got, "\n")+"\n") != sha(bundle) {
		t.Fatalf("what was committed is not what was served:\n%s", got)
	}
	k := dt.RemoteFile(t, h.remote, dt.CataloguePath("kustomization.yaml"))
	if strings.Count(k, "- shop.yaml") != 1 || !strings.Contains(k, "- path: shop.bundle.yaml") {
		t.Fatalf("kustomization:\n%s", k)
	}
	// Nothing else is written for a companion: no file of its own.
	listing := dt.Git(t, "", "--git-dir", h.remote, "ls-tree", "-r", "--name-only", "main", "clusters/"+dt.Cluster+"/catalogue/")
	for _, file := range strings.Fields(listing) {
		if base := filepath.Base(file); strings.Contains(base, "shop") && base != "shop.yaml" && base != "shop.bundle.yaml" {
			t.Fatalf("written beside the bundle: %s", file)
		}
	}
	var patch struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	carrier := dt.RemoteFile(t, h.remote, dt.CataloguePath(gitops.BundleFile("shop")))
	if err := yaml.Unmarshal([]byte(carrier), &patch); err != nil {
		t.Fatal(err)
	}
	carried, err := base64.StdEncoding.DecodeString(patch.Metadata.Annotations[profilebundle.Annotation])
	if err != nil || string(carried) != bundle {
		t.Fatalf("the carrier does not hold the whole bundle: %v\n%s", err, carried)
	}
	if patch.Kind != "ComponentProfile" || patch.Metadata.Name != "shop" ||
		patch.Metadata.Annotations[profilebundle.OriginAnnotation] != "cluster/gentian" {
		t.Fatalf("the carrier: %+v", patch)
	}
	// The pin is the digest of the whole file.
	if tenant := dt.RemoteFile(t, h.remote, dt.TenantPath("demo")); !strings.Contains(tenant, "  - profile: shop\n    digest: "+sha(bundle)+"\n") {
		t.Fatalf("the pin:\n%s", tenant)
	}
}

// A companion is an object of the whole cluster or of a namespace of the
// platform, and a Composition creates what it likes with the providers'
// rights. A tenant's own catalogue brings none: the install is refused,
// saying so, and nothing is written.
func TestATenantsOwnCatalogueBringsNoCompanions(t *testing.T) {
	bundle := shopBundle()
	alone := strings.SplitN(bundle, "---\n", 2)[0]
	src := catalogues(t, served{"gentian": {}, "demo-own": {"shop": bundle, "shop-alone": strings.ReplaceAll(strings.Replace(alone, "    composition: app-shop\n", "", 1), "shop", "shop-alone")}})
	h := startCatalogues(t, src)
	tom := h.token(t, "tenant-demo", "tom")
	delegate(t, h, "demo")
	if code, out := h.do(t, "PUT", "/v1/tenants/demo/catalogues/own", tom, urlBody(src.URL+"/demo-own")); code != http.StatusAccepted {
		t.Fatalf("add the catalogue: %d %v", code, out)
	}

	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/shop", tom, fmt.Sprintf(`{"coordinate":"own/shop","digest":%q}`, sha(bundle)))
	said := fmt.Sprint(out["error"])
	if code != http.StatusUnprocessableEntity || !strings.Contains(said, "Composition app-shop") ||
		!strings.Contains(said, "only a catalogue of the whole cluster") || !strings.Contains(said, "nothing was installed") {
		t.Fatalf("a Composition from a tenant's own catalogue: %d %v", code, out)
	}
	if catalogueHas(t, h, "shop.yaml") || strings.Contains(dt.RemoteFile(t, h.remote, dt.TenantPath("demo")), "profile: shop") {
		t.Fatal("something was written for a refused bundle")
	}
	// Its profiles it does bring.
	served := strings.ReplaceAll(strings.Replace(alone, "    composition: app-shop\n", "", 1), "shop", "shop-alone")
	if code, out := h.do(t, "POST", "/v1/tenants/demo/apps/shop-alone", tom, fmt.Sprintf(`{"coordinate":"own/shop-alone","digest":%q}`, sha(served))); code != http.StatusAccepted {
		t.Fatalf("a profile alone from a tenant's own catalogue: %d %v", code, out)
	}
}

// What is not on the list is refused whatever catalogue serves it, before
// anything is written.
func TestABundleHoldingWhatItMayNotIsRefused(t *testing.T) {
	cases := map[string]struct{ bundle, says string }{
		"a Secret":                      {shopBundle("apiVersion: v1\nkind: Secret\nmetadata:\n  name: shop\nstringData: {a: b}\n"), "Secret"},
		"a binding":                     {shopBundle("apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: shop\n"), "ClusterRoleBinding"},
		"the platform's Composition":    {strings.ReplaceAll(shopBundle(), "app-shop", "app-default"), "replaced by nothing"},
		"a Composition of another kind": {strings.Replace(shopBundle(), "kind: XApp", "kind: XTenant", 1), "composes XApp"},
		"a second profile":              {shopBundle(profileNamed("shop-two", "x")), "second ComponentProfile"},
		"a namespace of the platform":   {strings.Replace(shopBundle(), "  name: shop.portal-bridge-sso\n", "  name: shop.portal-bridge-sso\n  namespace: kernel-control\n", 1), "metadata.namespace"},
	}
	for what, c := range cases {
		src := catalogues(t, served{"gentian": {"shop": c.bundle}})
		h := startCatalogues(t, src)
		tom := h.token(t, "tenant-demo", "tom")
		code, out := h.do(t, "POST", "/v1/tenants/demo/apps/shop", tom, fmt.Sprintf(`{"coordinate":"gentian/shop","digest":%q}`, sha(c.bundle)))
		if said := fmt.Sprint(out["error"]); code != http.StatusUnprocessableEntity || !strings.Contains(said, c.says) || !strings.Contains(said, "nothing was installed") {
			t.Errorf("%s: %d %v", what, code, out)
		}
		if catalogueHas(t, h, "shop.yaml") {
			t.Errorf("%s: the bundle was committed", what)
		}
	}
}

// A bundle too large for its carrier is refused whole, companions or not:
// the limit is on the file.
func TestABundleTooLargeWithItsCompanionsIsRefused(t *testing.T) {
	filler := strings.Repeat("x", profilebundle.MaxBytes)
	bundle := strings.Replace(shopBundle(), `"<html></html>"`, `"`+filler+`"`, 1)
	src := catalogues(t, served{"gentian": {"shop": bundle}})
	h := startCatalogues(t, src)
	code, out := h.do(t, "POST", "/v1/tenants/demo/apps/shop", h.token(t, "tenant-demo", "tom"), fmt.Sprintf(`{"coordinate":"gentian/shop","digest":%q}`, sha(bundle)))
	if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(out["error"]), "too large") {
		t.Fatalf("a bundle past the carrier's limit: %d %v", code, out)
	}
	if catalogueHas(t, h, "shop.yaml") {
		t.Fatal("the bundle was committed")
	}
}

// The bundles gentian-apps builds install from a catalogue of the cluster,
// each at the digest of its whole file, and are committed byte for byte.
func TestTheBuiltBundlesInstall(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "profilebundle", "testdata", "bundles", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no built bundles: %v", err)
	}
	entries := map[string]string{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		entries[strings.TrimSuffix(filepath.Base(file), ".yaml")] = string(raw)
	}
	src := catalogues(t, served{"gentian": entries})
	h := startCatalogues(t, src)
	tom := h.token(t, "tenant-demo", "tom")
	for name, bundle := range entries {
		if strings.Contains(bundle, "gentianos.io/deployment-role: addon") {
			continue // an add-on is switched on in its base, not installed
		}
		code, out := h.do(t, "POST", "/v1/tenants/demo/apps/"+name, tom, fmt.Sprintf(`{"coordinate":"gentian/%s","digest":%q}`, name, sha(bundle)))
		if code != http.StatusAccepted {
			t.Errorf("%s: %d %v", name, code, out)
			continue
		}
		if got := dt.RemoteFile(t, h.remote, dt.CataloguePath(name+".yaml")); sha(strings.TrimRight(got, "\n")+"\n") != sha(strings.TrimRight(bundle, "\n")+"\n") {
			t.Errorf("%s: what was committed is not what was built", name)
		}
	}
}
