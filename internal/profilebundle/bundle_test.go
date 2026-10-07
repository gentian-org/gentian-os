/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// shop is a profile that names a Composition of its own and an OIDC client,
// and so may bring both.
const shop = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: shop
  labels:
    gentianos.io/profile-name: shop
spec:
  classes: [app]
  launch: tile
  trustTier: certified
  version: "1.0.0"
  package:
    chart: {repository: oci://example.invalid/shop, name: shop, version: "1.0.0"}
    composition: app-shop
  requires:
    services:
      identity:
        oidc:
          clientId: gentian-shop
`

const shopComposition = `apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: app-shop
  labels:
    gentianos.io/profile-name: shop
spec:
  compositeTypeRef:
    apiVersion: gentianos.io/v1alpha1
    kind: XApp
  mode: Pipeline
  pipeline:
    - step: render
      functionRef:
        name: function-go-templating
      input:
        apiVersion: gotemplating.fn.crossplane.io/v1beta1
        kind: GoTemplate
        source: Inline
        inline:
          template: |
            {{ .observed.composite.resource.metadata.name }}
`

const shopPacks = `apiVersion: gentianos.io/v1alpha1
kind: OIDCPackCatalog
metadata:
  name: shop-oidc
  labels:
    gentianos.io/profile-name: shop
spec:
  mapperTemplates:
    email:
      protocolMapper: oidc-usermodel-property-mapper
      config: {claim.name: email, "id.token.claim": "true"}
  packs:
    gentian-shop:
      scopeName: shop-scope
      clientRole: shop-access
      entitlementGroup: managed-by-attribute-Shop
      mappers: [email]
`

const shopPage = `apiVersion: v1
kind: ConfigMap
metadata:
  name: shop.portal-bridge-sso
  labels:
    gentianos.io/profile-name: shop
    gentianos.io/asset: portal-bridge-sso
data:
  sso.html: "<html>\n</html>\n"
`

const shopRecord = `apiVersion: gentianos.io/v1alpha1
kind: Customization
metadata:
  name: shop.own-entrypoint
  labels:
    gentianos.io/profile-name: shop
spec:
  summary: The chart's entrypoint is replaced
  target: {profile: shop}
  rung: L4
  scope: profile
  owner: platform-team
  reviewBy: "2027-02-06"
`

func stream(docs ...string) string { return strings.Join(docs, "---\n") }

var shopBundle = stream(shop, shopComposition, shopPage, shopRecord, shopPacks)

func TestAProfileAloneIsABundleWithNoCompanions(t *testing.T) {
	for _, origin := range []string{"cluster/main", "tenant/acme/own", ""} {
		b, err := Check([]byte(wiki), "wiki", origin)
		if err != nil {
			t.Fatalf("from %q: %v", origin, err)
		}
		if b.Name != "wiki" || len(b.Companions) != 0 {
			t.Fatalf("from %q: %+v", origin, b)
		}
	}
	// Separators and comments are not documents.
	if _, err := Check([]byte("# a comment\n---\n"+wiki+"\n---\n# nothing here\n"), "wiki", ""); err != nil {
		t.Fatal(err)
	}
}

func TestABundleBringsItsOwnCompanionsFromAClusterCatalogue(t *testing.T) {
	b, err := Check([]byte(shopBundle), "shop", ClusterOrigin("main"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range b.Companions {
		got = append(got, c.String())
	}
	want := "Composition app-shop, ConfigMap shop.portal-bridge-sso, Customization shop.own-entrypoint, OIDCPackCatalog shop-oidc"
	if strings.Join(got, ", ") != want {
		t.Fatalf("companions: %v", got)
	}
	if b.Composition() != "app-shop" {
		t.Fatalf("composition: %q", b.Composition())
	}
	for _, c := range b.Companions {
		if namespaced := c.Kind == KindConfigMap || c.Kind == KindCustomization; c.Namespaced != namespaced {
			t.Fatalf("%s: namespaced %v", c, c.Namespaced)
		}
	}
}

// Every companion is an object of the whole cluster or of a namespace of the
// platform. A tenant's own catalogue publishes profiles and nothing else, and
// neither does a profile nobody recorded an origin for.
func TestOnlyAClusterCatalogueBringsCompanions(t *testing.T) {
	for _, companion := range []string{shopComposition, shopPacks, shopPage, shopRecord} {
		for _, origin := range []string{TenantOrigin("acme", "own"), "", "somewhere/else/entirely/x"} {
			_, err := Check([]byte(stream(shop, companion)), "shop", origin)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "only a catalogue of the whole cluster") {
				t.Fatalf("from %q: %v", origin, err)
			}
		}
	}
}

func TestWhatABundleMayNotHoldIsRefused(t *testing.T) {
	swap := func(doc, old, new string) string {
		t.Helper()
		if !strings.Contains(doc, old) {
			t.Fatalf("%q is not in the document", old)
		}
		return strings.Replace(doc, old, new, 1)
	}
	kind := func(apiVersion, kind, rest string) string {
		return "apiVersion: " + apiVersion + "\nkind: " + kind + "\nmetadata:\n  name: shop\n  labels:\n    gentianos.io/profile-name: shop\n" + rest
	}
	cases := []struct {
		what   string
		bundle string
		says   string
	}{
		{"nothing at all", "# only a comment\n", "no document"},
		{"something that is not a profile", shopPage, "not a ComponentProfile"},
		{"a profile of another name", swap(shop, "name: shop\n", "name: store\n"), `named "store"`},
		{"a profile second", stream(shopPage, shop), "not a ComponentProfile"},
		{"a second profile", stream(shop, swap(wiki, "name: wiki", "name: shop-two")), "second ComponentProfile"},
		{"a profile wearing another's label", swap(shop, "gentianos.io/profile-name: shop", "gentianos.io/profile-name: nextcloud-base-ce"), "another profile's name"},
		{"a profile with a sync option", swap(shop, "  labels:\n", "  annotations:\n    argocd.argoproj.io/sync-options: Replace=true\n  labels:\n"), "addressed to Argo CD"},

		// Kinds.
		{"a Secret", stream(shop, kind("v1", "Secret", "stringData: {a: b}\n")), "not a kind a bundle may hold"},
		{"a Namespace", stream(shop, kind("v1", "Namespace", "")), "not a kind a bundle may hold"},
		{"a ClusterRole", stream(shop, kind("rbac.authorization.k8s.io/v1", "ClusterRole", "rules: []\n")), "not a kind a bundle may hold"},
		{"a ClusterRoleBinding", stream(shop, kind("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "")), "not a kind a bundle may hold"},
		{"a CustomResourceDefinition", stream(shop, kind("apiextensions.k8s.io/v1", "CustomResourceDefinition", "")), "not a kind a bundle may hold"},
		{"a webhook", stream(shop, kind("admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration", "")), "not a kind a bundle may hold"},
		{"an AppPackage", stream(shop, kind("gentianos.io/v1alpha1", "AppPackage", "spec: {family: shop}\n")), "not a kind a bundle may hold"},
		{"a Deployment", stream(shop, kind("apps/v1", "Deployment", "")), "not a kind a bundle may hold"},
		{"a definition of composites", stream(shop, kind("apiextensions.crossplane.io/v1", "CompositeResourceDefinition", "")), "not a kind a bundle may hold"},
		{"a Composition of another version", stream(shop, swap(shopComposition, "apiextensions.crossplane.io/v1\n", "apiextensions.crossplane.io/v1beta1\n")), "not a kind a bundle may hold"},
		{"a document with no kind", stream(shop, "metadata:\n  name: shop\n"), "not a kind a bundle may hold"},
		{"a document that is a list", stream(shop, "- a\n- b\n"), "does not parse"},

		// Compositions.
		{"the platform's Composition", stream(swap(shop, "app-shop", "app-default"), swap(shopComposition, "name: app-shop", "name: app-default")), "replaced by nothing"},
		{"another app's Composition", stream(swap(shop, "composition: app-shop", "composition: app-odoo-base-ce"), swap(shopComposition, "name: app-shop", "name: app-odoo-base-ce")), "the Composition of profile shop is app-shop"},
		{"a Composition of a tenant", stream(shop, swap(shopComposition, "kind: XApp", "kind: XTenant")), "composes XApp"},
		{"a Composition of a cluster", stream(shop, swap(shopComposition, "kind: XApp", "kind: XCluster")), "composes XApp"},
		{"a Composition of another group", stream(shop, swap(shopComposition, "apiVersion: gentianos.io/v1alpha1\n    kind: XApp", "apiVersion: example.org/v1\n    kind: XApp")), "composes XApp"},
		{"a Composition the profile does not name", stream(swap(shop, "    composition: app-shop\n", ""), shopComposition), "not the one its profile is rendered by"},
		{"two Compositions", stream(shop, shopComposition, shopComposition), "twice"},

		// Ownership.
		{"packs under another name", stream(shop, swap(shopPacks, "name: shop-oidc", "name: nextcloud-base-ce-oidc")), "that of profile shop is shop-oidc"},
		{"the platform's packs", stream(shop, swap(shopPacks, "name: shop-oidc", "name: gentian-kernel")), "that of profile shop is shop-oidc"},
		{"a pack for another app's client", stream(shop, swap(shopPacks, "    gentian-shop:", "    gentian-nextcloud-base-ce:")), "does not declare"},
		{"a pack for a service client", stream(shop, swap(shopPacks, "      scopeName: shop-scope", "      serviceClient: true")), "the platform's to declare"},
		{"a ConfigMap of the platform's name", stream(shop, swap(shopPage, "name: shop.portal-bridge-sso", "name: gentian-cluster-config")), "is shop.portal-bridge-sso"},
		{"a ConfigMap of another profile's", stream(shop, swap(shopPage, "name: shop.portal-bridge-sso", "name: wiki.portal-bridge-sso")), "is shop.portal-bridge-sso"},
		{"a ConfigMap that says it is the cluster's configuration", stream(shop, swap(shopPage, "  labels:\n", "  labels:\n    gentianos.io/config-type: cluster-config\n")), "exactly these labels"},
		{"a ConfigMap with another profile's label", stream(shop, swap(shopPage, "gentianos.io/profile-name: shop", "gentianos.io/profile-name: wiki")), "exactly these labels"},
		{"a ConfigMap with no asset", stream(shop, swap(shopPage, "    gentianos.io/asset: portal-bridge-sso\n", "")), "needs the label gentianos.io/asset"},
		{"a ConfigMap with something other than text", stream(shop, swap(shopPage, `"<html>\n</html>\n"`, "{a: b}")), "other than text"},
		{"a record about another app", stream(shop, swap(shopRecord, "target: {profile: shop}", "target: {profile: wiki}")), "not about the profile it travels with"},
		{"a record of the platform's scope", stream(shop, swap(shopRecord, "scope: profile", "scope: platform")), "records of scope profile"},
		{"a record under a bare name", stream(shop, swap(shopRecord, "name: shop.own-entrypoint", "name: own-entrypoint")), "shop.<record>"},

		// Shape: where it goes and what it says about itself is not its to say.
		{"a namespace", stream(shop, swap(shopPage, "metadata:\n", "metadata:\n  namespace: kernel-control\n")), "metadata.namespace"},
		{"a tenant's namespace", stream(shop, swap(shopRecord, "metadata:\n", "metadata:\n  namespace: tenant-acme\n")), "metadata.namespace"},
		{"an annotation", stream(shop, swap(shopComposition, "metadata:\n", "metadata:\n  annotations: {argocd.argoproj.io/sync-options: Prune=false}\n")), "metadata.annotations"},
		{"a finalizer", stream(shop, swap(shopPacks, "metadata:\n", "metadata:\n  finalizers: [x]\n")), "metadata.finalizers"},
		{"an owner", stream(shop, swap(shopPage, "metadata:\n", "metadata:\n  ownerReferences: []\n")), "metadata.ownerReferences"},
		{"a status", stream(shop, shopPacks+"status: {packCount: 1}\n"), "states status"},
		{"binary data", stream(shop, shopPage+"binaryData: {a: YQ==}\n"), "states binaryData"},
		{"an immutable ConfigMap", stream(shop, shopPage+"immutable: true\n"), "states immutable"},
		{"the same companion twice", stream(shop, shopPage, shopPage), "twice"},
	}
	for _, c := range cases {
		_, err := Check([]byte(c.bundle), "shop", ClusterOrigin("main"))
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", c.what, err)
			continue
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: the refusal does not say %q: %v", c.what, c.says, err)
		}
	}
}

// A companion's name is the profile's with a dot or a fixed word around it,
// which works because a profile's name has neither. A profile whose name is
// not of that kind brings nothing.
func TestAProfileWithAnOddNameBringsNoCompanions(t *testing.T) {
	odd := strings.ReplaceAll(shop, "shop", "my.shop")
	if _, err := Check([]byte(odd), "my.shop", "cluster/main"); err != nil {
		t.Fatalf("alone: %v", err)
	}
	page := strings.ReplaceAll(shopPage, "shop", "my.shop")
	if _, err := Check([]byte(stream(odd, page)), "my.shop", "cluster/main"); !errors.Is(err, ErrRefused) {
		t.Fatalf("with a companion: %v", err)
	}
}

// The names a companion may have are safe because of what the platform's own
// objects are called. This holds the platform to that: the only Composition
// of its own beginning app- is the one nothing replaces, and its own OIDC
// packs do not end the way a profile's do.
func TestThePlatformsOwnObjectsHaveNoCompanionsName(t *testing.T) {
	root := filepath.Join("..", "..")
	files, err := filepath.Glob(filepath.Join(root, "crossplane", "compositions", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no compositions found: %v", err)
	}
	found := false
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		name := doc.Metadata.Name
		if name == defaultComposition {
			found = true
			continue
		}
		if strings.HasPrefix(name, compositionPrefix) {
			t.Errorf("%s: the platform's Composition %s has the name a profile called %q would give its own; "+
				"refuse it by name in checkComposition", file, name, strings.TrimPrefix(name, compositionPrefix))
		}
	}
	if !found {
		t.Errorf("no %s among the platform's compositions: the name checkComposition protects is not one", defaultComposition)
	}

	packs, err := os.ReadFile(filepath.Join(root, "charts", "gentian-os", "templates", "oidcpackcatalog-kernel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(packs), "\n") {
		if name, ok := strings.CutPrefix(line, "  name: "); ok && strings.HasSuffix(strings.TrimSpace(name), oidcSuffix) {
			t.Errorf("the platform's OIDCPackCatalog %s has the name a profile's would", name)
		}
	}
}

// OIDCPackCatalog and Customization are compared exactly, which is right
// only while their schemas fill nothing in: a default would be a field in
// the cluster that no bundle states. Whoever adds one gives companions.go
// the schema to apply it from, as the profile has.
func TestTheComparedKindsStateNoDefaults(t *testing.T) {
	for _, file := range []string{"gentianos.io_oidcpackcatalogs.yaml", "gentianos.io_customizations.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", file))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "default:") {
				t.Errorf("%s:%d states a default, and a companion of this kind is compared without them", file, i+1)
			}
		}
	}
}
