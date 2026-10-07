/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// shopBundle is a bundle that brings one companion of each kind.
const shopBundle = `apiVersion: gentianos.io/v1alpha1
kind: ComponentProfile
metadata:
  name: shop
spec:
  classes: [app]
  launch: none
  trustTier: certified
  version: "1.0.0"
  package:
    composition: app-shop
    chart: {repository: oci://example.invalid/charts, name: shop, version: "1.0.0"}
  requires:
    identity:
      clients:
        - clientId: shop
---
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: app-shop
  labels:
    gentianos.io/profile-name: shop
spec:
  compositeTypeRef: {apiVersion: gentianos.io/v1alpha1, kind: XApp}
---
apiVersion: gentianos.io/v1alpha1
kind: OIDCPackCatalog
metadata:
  name: shop-oidc
  labels:
    gentianos.io/profile-name: shop
spec:
  packs:
    shop:
      scopeName: shop
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: shop.theme
  labels:
    gentianos.io/profile-name: shop
    gentianos.io/asset: theme
data:
  theme.css: "body {}"
---
apiVersion: gentianos.io/v1alpha1
kind: Customization
metadata:
  name: shop.patch
  labels:
    gentianos.io/profile-name: shop
spec:
  target: {profile: shop}
  scope: profile
`

// alone is a bundle that is its profile and nothing else.
func alone(name string) string {
	return "apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: " + name +
		"\nspec:\n  classes: [app]\n  launch: none\n  trustTier: certified\n  version: \"1.0.0\"\n"
}

// carrying is a profile on the cluster as the director materialised it: the
// bundle beside it, from a catalogue of the whole cluster.
func carrying(p *gentianov1alpha1.ComponentProfile, bundle string) *gentianov1alpha1.ComponentProfile {
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[profilebundle.Annotation] = profilebundle.Encode([]byte(bundle))
	p.Annotations[profilebundle.OriginAnnotation] = profilebundle.ClusterOrigin("main")
	return p
}

func materialisedProfile(name, bundle string) *gentianov1alpha1.ComponentProfile {
	return carrying(&gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}, bundle)
}

const provisioningNS = "kernel-provisioning"

// object is something of a companion kind on the cluster.
func object(apiVersion, kind, namespace, name string, labels map[string]string, body map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	for key, value := range body {
		u.Object[key] = value
	}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	u.SetNamespace(namespace)
	u.SetLabels(labels)
	return u
}

func composition(name, composes string, labels map[string]string) *unstructured.Unstructured {
	return object("apiextensions.crossplane.io/v1", "Composition", "", name, labels, map[string]any{
		"spec": map[string]any{"compositeTypeRef": map[string]any{"apiVersion": "gentianos.io/v1alpha1", "kind": composes}},
	})
}

func packs(name string, labels map[string]string, clients ...string) *unstructured.Unstructured {
	held := map[string]any{}
	for _, c := range clients {
		held[c] = map[string]any{"scopeName": c}
	}
	return object("gentianos.io/v1alpha1", "OIDCPackCatalog", "", name, labels, map[string]any{"spec": map[string]any{"packs": held}})
}

func configMap(namespace, name string, labels map[string]string) *unstructured.Unstructured {
	return object("v1", "ConfigMap", namespace, name, labels, map[string]any{"data": map[string]any{"k": "v"}})
}

func customization(namespace, name, about string, labels map[string]string) *unstructured.Unstructured {
	return object("gentianos.io/v1alpha1", "Customization", namespace, name, labels, map[string]any{
		"spec": map[string]any{"target": map[string]any{"profile": about}, "scope": "profile"},
	})
}

func of(profile string) map[string]string {
	return map[string]string{profilebundle.ProfileLabel: profile}
}

// argoCatalogue is the Application that applies the catalogue directory,
// reporting what Argo CD found of the objects named: "kind/name" for one it
// finds declared, "kind/name!" for one it would prune.
func argoCatalogue(reports ...string) *unstructured.Unstructured {
	resources := []any{}
	for _, report := range reports {
		ref, prune := strings.CutSuffix(report, "!")
		kind, name, _ := strings.Cut(ref, "/")
		entry := map[string]any{"kind": kind, "name": name, "group": "gentianos.io", "version": "v1alpha1"}
		switch kind {
		case "ConfigMap":
			entry["group"], entry["version"], entry["namespace"] = "", "v1", provisioningNS
		case "Customization":
			entry["namespace"] = provisioningNS
		case "Composition":
			entry["group"], entry["version"] = "apiextensions.crossplane.io", "v1"
		}
		if prune {
			entry["requiresPruning"] = true
		}
		resources = append(resources, entry)
	}
	app := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"resources": resources}}}
	app.SetGroupVersionKind(applicationGVK)
	app.SetName("gentian-catalogue-dev")
	app.SetNamespace(layout.Namespace(layout.GitOps))
	return app
}

// residueWorld is a cluster after some history. Tenant demo has shop
// installed, with an add-on, and drive; it uninstalled wiki and kept its
// data. Around them is everything the catalogue leaves behind, beside
// everything that must never be taken for it.
//
// On the list, by class:
//
//	dropped         ConfigMap shop.old-page (shop's label; shop's bundle no longer brings it)
//	                Composition app-notes (the name notes' Composition would have; notes brings none)
//	orphaned        OIDCPackCatalog gone-oidc (labelled for a profile that is not there)
//	unowned         Composition app-odoo, OIDCPackCatalog gentian-element-oidc,
//	                ConfigMap legacy.asset, ConfigMap desktop.page (its profile has no bundle),
//	                Composition shop-composition (an app's, under a name no bundle gives)
//	unused-profile  ComponentProfile notes
func residueWorld(t *testing.T, more ...client.Object) *purgeWorld {
	t.Helper()
	tenant := demoTenant()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{
		{Profile: "drive"},
		{Profile: "shop", Addons: []string{"shop-gift"}},
	}
	desktop := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "desktop", Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}},
		Spec:       gentianov1alpha1.ComponentProfileSpec{DefaultForTenants: true},
	}
	objects := []client.Object{
		// Profiles: in use, an add-on in use, retained, shipped by the
		// chart, and one nobody uses.
		materialisedProfile("shop", shopBundle),
		materialisedProfile("shop-gift", alone("shop-gift")),
		materialisedProfile("drive", alone("drive")),
		carrying(wikiProfile(), alone("wiki")),
		materialisedProfile("notes", alone("notes")),
		desktop,
		component("shop", "Ready", "", true),
		component("drive", "Ready", "", true),

		// What shop's bundle brings: live companions.
		composition("app-shop", "XApp", of("shop")),
		packs("shop-oidc", of("shop"), "shop"),
		configMap(provisioningNS, "shop.theme", map[string]string{profilebundle.ProfileLabel: "shop", profilebundle.AssetLabel: "theme"}),
		customization(provisioningNS, "shop.patch", "shop", of("shop")),

		// The platform's own.
		composition("app-default", "XApp", map[string]string{"gentianos.io/composition": "app"}),
		composition("tenant-default", "XTenant", of("shop")),
		object("gentianos.io/v1alpha1", "OIDCPackCatalog", "", "gentian-kernel",
			map[string]string{"app.kubernetes.io/managed-by": "Helm"},
			map[string]any{"spec": map[string]any{"packs": map[string]any{"dovecot": map[string]any{"serviceClient": true}}}}),
		configMap(provisioningNS, "kube-root-ca.crt", nil),
		configMap(provisioningNS, "gentian-cluster-config", map[string]string{"gentianos.io/config-type": "cluster-config"}),
		customization(provisioningNS, "hand-written", "shop", nil),

		// A tenant's namespace: the same names and labels, and not the
		// catalogue's business.
		configMap("tenant-demo", "shop.old-page", map[string]string{profilebundle.ProfileLabel: "shop", profilebundle.AssetLabel: "old-page"}),
		customization("tenant-demo", "shop.local", "shop", of("shop")),

		// Residue.
		configMap(provisioningNS, "shop.old-page", map[string]string{profilebundle.ProfileLabel: "shop", profilebundle.AssetLabel: "old-page"}),
		composition("app-notes", "XApp", nil),
		packs("gone-oidc", of("gone"), "gone"),
		composition("app-odoo", "XApp", nil),
		packs("gentian-element-oidc", nil, "shop"),
		configMap(provisioningNS, "legacy.asset", map[string]string{profilebundle.AssetLabel: "asset"}),
		configMap(provisioningNS, "desktop.page", map[string]string{profilebundle.ProfileLabel: "desktop", profilebundle.AssetLabel: "page"}),
		composition("shop-composition", "XApp", nil),
	}
	hasArgo := false
	for _, o := range more {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "Application" {
			hasArgo = true
		}
	}
	if !hasArgo {
		// Argo CD as it is with nothing pending: it tracks what the
		// directory declares, and nothing of what is left over.
		objects = append(objects, argoCatalogue("ComponentProfile/shop", "ComponentProfile/notes",
			"Composition/app-shop", "ConfigMap/shop.theme"))
	}
	return newPurgeWorld(t, tenant, append(objects, more...))
}

func residueOf(t *testing.T, w *purgeWorld) (*CatalogueResidue, map[string]ResidueItem) {
	t.Helper()
	got, err := w.svc.CatalogueResidue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ResidueItem{}
	for _, item := range got.Residue {
		by[item.Kind+"/"+item.Name] = item
	}
	if len(by) != len(got.Residue) {
		t.Fatalf("an object is listed twice: %+v", got.Residue)
	}
	return got, by
}

// The list holds each class of residue, and nothing else: no companion a
// bundle on the cluster brings, not the platform's Composition or pack
// catalog, no profile the chart ships or a tenant uses or retains data for,
// and nothing in a tenant's namespace.
func TestTheResidueListHoldsEachClassAndNothingThatIsOwned(t *testing.T) {
	w := residueWorld(t)
	got, by := residueOf(t, w)

	want := map[string]string{
		"ConfigMap/shop.old-page":              ResidueDropped,
		"Composition/app-notes":                ResidueDropped,
		"OIDCPackCatalog/gone-oidc":            ResidueOrphaned,
		"Composition/app-odoo":                 ResidueUnowned,
		"OIDCPackCatalog/gentian-element-oidc": ResidueUnowned,
		"ConfigMap/legacy.asset":               ResidueUnowned,
		"ConfigMap/desktop.page":               ResidueUnowned,
		"Composition/shop-composition":         ResidueUnowned,
		"ComponentProfile/notes":               ResidueUnusedProfile,
	}
	classes := map[string]string{}
	for key, item := range by {
		classes[key] = item.Class
	}
	if !reflect.DeepEqual(classes, want) {
		t.Fatalf("listed:\n %v\nwant:\n %v", classes, want)
	}
	if len(got.Incomplete) != 0 {
		t.Errorf("everything could be read and the answer says incomplete: %v", got.Incomplete)
	}
	if got.Namespace != provisioningNS || got.OIDCRule == "" {
		t.Errorf("namespace %q, rule %q", got.Namespace, got.OIDCRule)
	}

	// Each says which profile it names, where it is, and why it is listed.
	page := by["ConfigMap/shop.old-page"]
	if page.Profile != "shop" || page.Namespace != provisioningNS || !strings.Contains(page.Reason, "does not bring it") {
		t.Errorf("dropped by label: %+v", page)
	}
	if byName := by["Composition/app-notes"]; byName.Profile != "notes" || byName.Namespace != "" ||
		!strings.Contains(byName.Reason, "its name is the one a Composition of profile notes has") {
		t.Errorf("dropped by name: %+v", byName)
	}
	if gone := by["OIDCPackCatalog/gone-oidc"]; gone.Profile != "gone" || !strings.Contains(gone.Reason, "no profile gone is on this cluster") {
		t.Errorf("orphaned: %+v", gone)
	}
	for _, key := range []string{"Composition/app-odoo", "OIDCPackCatalog/gentian-element-oidc", "ConfigMap/legacy.asset", "ConfigMap/desktop.page"} {
		if !strings.HasPrefix(by[key].Reason, "not owned by any bundle") {
			t.Errorf("%s is not marked as owned by no bundle: %q", key, by[key].Reason)
		}
	}
	if by["ConfigMap/desktop.page"].Profile != "desktop" || by["Composition/app-odoo"].Profile != "" {
		t.Errorf("profiles of the unowned: %+v, %+v", by["ConfigMap/desktop.page"], by["Composition/app-odoo"])
	}
	if unused := by["ComponentProfile/notes"]; !strings.HasPrefix(unused.Reason, "unused profile") || !unused.Removable {
		t.Errorf("unused profile: %+v", unused)
	}
	// A Composition is removed only under a name a bundle gives one.
	if odd := by["Composition/shop-composition"]; odd.Removable || odd.NotRemovable == "" {
		t.Errorf("a Composition not named app-<profile> is offered for removal: %+v", odd)
	}
	if !by["Composition/app-odoo"].Removable {
		t.Error("app-odoo is not removable")
	}
}

// A pack catalog on the list says whether it is still read: the resolver
// takes the first catalog holding a client id, whoever brought it.
func TestAResidualPackCatalogSaysWhetherItIsStillInEffect(t *testing.T) {
	w := residueWorld(t,
		// Labelled for shop, which is installed and has its own catalog:
		// the Composition asks for the one carrying shop's label.
		packs("shop-old-oidc", of("shop"), "shop-admin"),
		// Holds nothing.
		packs("empty-oidc", of("gone")),
	)
	_, by := residueOf(t, w)

	// The only catalog holding "gone".
	if o := by["OIDCPackCatalog/gone-oidc"].OIDC; o == nil || o.Effective != EffectiveYes ||
		!reflect.DeepEqual(o.Clients, []string{"gone"}) || len(o.Contested) != 0 {
		t.Errorf("a sole holder: %+v", o)
	}
	// Holds "shop", which shop's own catalog holds too.
	if o := by["OIDCPackCatalog/gentian-element-oidc"].OIDC; o == nil || o.Effective != EffectiveContested ||
		!reflect.DeepEqual(o.Contested, []string{"shop"}) || len(o.Clients) != 0 {
		t.Errorf("a second holder: %+v", o)
	}
	// The only holder of shop-admin: in effect, and also read by label.
	if o := by["OIDCPackCatalog/shop-old-oidc"].OIDC; o == nil || o.Effective != EffectiveYes || o.Composition != "shop" {
		t.Errorf("labelled for an installed profile: %+v", o)
	}
	if o := by["OIDCPackCatalog/empty-oidc"].OIDC; o == nil || o.Effective != EffectiveNo {
		t.Errorf("a catalog holding nothing: %+v", o)
	}
	if by["Composition/app-odoo"].OIDC != nil {
		t.Error("a Composition says something about OIDC")
	}
}

// Whether a profile is unused depends on whether a tenant retains data for
// it, and that is the retained-data read's to say. Where it could not say,
// no profile is listed as unused, and the answer says what was not known.
func TestNoProfileIsCalledUnusedWhenRetainedDataCannotBeEstablished(t *testing.T) {
	w := residueWorld(t)
	w.svc.vault = nil
	got, by := residueOf(t, w)
	if _, listed := by["ComponentProfile/notes"]; listed {
		t.Fatal("a profile is listed as unused although what tenants retain could not be read")
	}
	if len(got.Incomplete) != 1 || !strings.Contains(got.Incomplete[0], "unused profiles are not listed") {
		t.Fatalf("incomplete = %v", got.Incomplete)
	}
	// The rest of the list does not depend on it.
	if _, listed := by["Composition/app-odoo"]; !listed {
		t.Error("the companions are missing too")
	}
}

// A profile whose bundle cannot be read owns nobody knows what: nothing
// that names it is listed, and the answer says so.
func TestNothingNamingAProfileWithAnUnreadableBundleIsListed(t *testing.T) {
	broken := materialisedProfile("broken", alone("broken"))
	broken.Annotations[profilebundle.Annotation] = "!!! not base64"
	w := residueWorld(t, broken,
		configMap(provisioningNS, "broken.page", map[string]string{profilebundle.ProfileLabel: "broken", profilebundle.AssetLabel: "page"}),
		composition("app-broken", "XApp", nil),
	)
	got, by := residueOf(t, w)
	for _, key := range []string{"ConfigMap/broken.page", "Composition/app-broken"} {
		if _, listed := by[key]; listed {
			t.Errorf("%s is listed although the bundle that might bring it cannot be read", key)
		}
	}
	if len(got.Incomplete) == 0 || !strings.Contains(got.Incomplete[0], "broken") {
		t.Fatalf("incomplete = %v", got.Incomplete)
	}
}

// everything is every object of the kinds a removal could touch, by name.
func everything(t *testing.T, w *purgeWorld) []string {
	t.Helper()
	var out []string
	kinds := profilebundle.CompanionKinds()
	for _, kind := range kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind.GVK.GroupVersion().WithKind(kind.Kind + "List"))
		if err := w.objs.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		for _, item := range list.Items {
			out = append(out, kind.Kind+"/"+item.GetNamespace()+"/"+item.GetName())
		}
	}
	var profiles gentianov1alpha1.ComponentProfileList
	if err := w.objs.List(context.Background(), &profiles); err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles.Items {
		out = append(out, "ComponentProfile//"+p.Name)
	}
	sort.Strings(out)
	return out
}

// A removal deletes the one object named, and no other.
func TestARemovalDeletesExactlyTheOneObjectNamed(t *testing.T) {
	w := residueWorld(t)
	before := everything(t, w)

	res, err := w.svc.RemoveResidue(context.Background(), "ConfigMap", "shop.old-page", "", "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "deleted" || res.Deleted == nil || res.Deleted.Name != "shop.old-page" ||
		res.Deleted.Class != ResidueDropped || !strings.Contains(res.Message, "ada@example.com") {
		t.Fatalf("answer = %+v", res)
	}
	var want []string
	for _, name := range before {
		// The one in the catalogue's namespace; the tenant's of the same
		// name stays.
		if name != "ConfigMap/"+provisioningNS+"/shop.old-page" {
			want = append(want, name)
		}
	}
	if after := everything(t, w); !reflect.DeepEqual(after, want) {
		t.Fatalf("after the removal:\n %v\nwant:\n %v", after, want)
	}

	// A Composition nobody owns, under a name a bundle gives one.
	if res, err := w.svc.RemoveResidue(context.Background(), "Composition", "app-odoo", "", "ada"); err != nil || res.Status != "deleted" {
		t.Fatalf("app-odoo: %+v, %v", res, err)
	}
	// Given the catalogue's namespace by name, a namespaced one is found.
	if res, err := w.svc.RemoveResidue(context.Background(), "ConfigMap", "legacy.asset", provisioningNS, "ada"); err != nil || res.Status != "deleted" {
		t.Fatalf("legacy.asset: %+v, %v", res, err)
	}
}

// Anything that is not on the list is refused, with the reason, and stays.
func TestARemovalRefusesWhatIsNotOnTheList(t *testing.T) {
	w := residueWorld(t)
	before := everything(t, w)

	for _, c := range []struct {
		kind, name, namespace string
		is                    error
		says                  string
	}{
		// What a bundle on the cluster brings.
		{"Composition", "app-shop", "", ErrNotResidue, "the bundle now materialised for profile shop brings it"},
		{"OIDCPackCatalog", "shop-oidc", "", ErrNotResidue, "profile shop brings it"},
		{"ConfigMap", "shop.theme", "", ErrNotResidue, "profile shop brings it"},
		{"Customization", "shop.patch", "", ErrNotResidue, "profile shop brings it"},
		// The platform's.
		{"Composition", "app-default", "", ErrNotResidue, "the platform's own Composition"},
		{"Composition", "tenant-default", "", ErrNotResidue, "does not compose the platform's app"},
		{"OIDCPackCatalog", "gentian-kernel", "", ErrNotResidue, "service client"},
		{"ConfigMap", "gentian-cluster-config", "", ErrNotResidue, "neither a profile's label nor an asset's"},
		{"ConfigMap", "kube-root-ca.crt", "", ErrNotResidue, "neither a profile's label nor an asset's"},
		{"Customization", "hand-written", "", ErrNotResidue, "no profile's label"},
		// A tenant's namespace, named outright or not.
		{"ConfigMap", "shop.old-page", "tenant-demo", ErrNotResidue, "nothing in another namespace is ever removed"},
		{"Customization", "shop.local", "", ErrNotResidue, "there is no Customization shop.local"},
		{"Composition", "app-odoo", provisioningNS, ErrNotResidue, "has no namespace"},
		// Listed, and not removed here.
		{"Composition", "shop-composition", "", ErrNotResidue, "only a Composition named app-<profile>"},
		// Not there, and not a kind.
		{"ConfigMap", "nothing.here", "", ErrNotResidue, "there is no ConfigMap nothing.here"},
		{"Secret", "keycloak-admin", "", ErrNotARemovableKind, "not a kind"},
		{"Tenant", "demo", "", ErrNotARemovableKind, "not a kind"},
		// Profiles in use, retained, the platform's, and not there.
		{"ComponentProfile", "shop", "", ErrNotResidue, "is in use"},
		{"ComponentProfile", "shop-gift", "", ErrNotResidue, "is in use"},
		{"ComponentProfile", "wiki", "", ErrNotResidue, "a tenant retains data for it"},
		{"ComponentProfile", "desktop", "", ErrNotResidue, "not materialised from a catalogue"},
		{"ComponentProfile", "absent", "", ErrNotResidue, "there is no ComponentProfile absent"},
	} {
		res, err := w.svc.RemoveResidue(context.Background(), c.kind, c.name, c.namespace, "ada")
		if res != nil || !errors.Is(err, c.is) {
			t.Errorf("%s %s (%s): %+v, %v; want %v", c.kind, c.name, c.namespace, res, err, c.is)
			continue
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s %s is refused without saying %q: %v", c.kind, c.name, c.says, err)
		}
	}
	if after := everything(t, w); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused removal deleted something:\n %v\nwas:\n %v", after, before)
	}
}

// An object a new install adopted after the list was read is not residue
// when the removal is asked: the list is worked out again there.
func TestARemovalIsRefusedForAnObjectABundleHasJustAdopted(t *testing.T) {
	w := residueWorld(t)
	if _, by := residueOf(t, w); by["Composition/app-notes"].Class != ResidueDropped {
		t.Fatal("app-notes is not listed to begin with")
	}
	// notes moves to a build that brings the Composition.
	bundle := strings.Replace(alone("notes"), "  version: \"1.0.0\"\n", "  version: \"1.0.0\"\n  package:\n    composition: app-notes\n", 1) +
		"---\napiVersion: apiextensions.crossplane.io/v1\nkind: Composition\nmetadata:\n  name: app-notes\n  labels:\n" +
		"    gentianos.io/profile-name: notes\nspec:\n  compositeTypeRef: {apiVersion: gentianos.io/v1alpha1, kind: XApp}\n"
	notes := &gentianov1alpha1.ComponentProfile{}
	if err := w.objs.Get(context.Background(), client.ObjectKey{Name: "notes"}, notes); err != nil {
		t.Fatal(err)
	}
	if err := w.objs.Update(context.Background(), carrying(notes, bundle)); err != nil {
		t.Fatal(err)
	}
	_, err := w.svc.RemoveResidue(context.Background(), "Composition", "app-notes", "", "ada")
	if !errors.Is(err, ErrNotResidue) || !strings.Contains(err.Error(), "profile notes brings it") {
		t.Fatalf("err = %v", err)
	}
	if !exists(t, w, "apiextensions.crossplane.io/v1", "Composition", "", "app-notes") {
		t.Fatal("the adopted Composition was deleted")
	}
}

// touchBeforeDelete writes to an object after it was checked and before it
// is deleted: what Argo CD applying it again in that moment looks like.
type touchBeforeDelete struct {
	client.Client
}

func (c touchBeforeDelete) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
		return err
	}
	current.SetAnnotations(map[string]string{"touched": "meanwhile"})
	if err := c.Update(ctx, current); err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

// The deletion names the object as it was checked. One that changed in
// between is not deleted, and the answer says nothing was.
func TestARemovalThatLosesARaceDeletesNothing(t *testing.T) {
	w := residueWorld(t)
	w.svc.client = touchBeforeDelete{w.objs}
	res, err := w.svc.RemoveResidue(context.Background(), "ConfigMap", "shop.old-page", "", "ada")
	if res != nil || !errors.Is(err, ErrResidueChanged) || !strings.Contains(err.Error(), "Nothing was deleted") {
		t.Fatalf("answer = %+v, %v", res, err)
	}
	if !exists(t, w, "v1", "ConfigMap", provisioningNS, "shop.old-page") {
		t.Fatal("an object that changed after it was checked was deleted")
	}
}

func exists(t *testing.T, w *purgeWorld, apiVersion, kind, namespace, name string) bool {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	err := w.objs.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// What Argo CD still finds declared in the catalogue directory is not
// deleted, and neither is anything while Argo CD cannot be asked.
func TestARemovalIsRefusedForWhatArgoStillFindsDeclared(t *testing.T) {
	w := residueWorld(t, argoCatalogue("ConfigMap/shop.old-page", "Composition/app-odoo!"))
	_, err := w.svc.RemoveResidue(context.Background(), "ConfigMap", "shop.old-page", "", "ada")
	if !errors.Is(err, ErrStillDeclared) || !strings.Contains(err.Error(), "gentian-catalogue-dev") {
		t.Fatalf("declared: %v", err)
	}
	if !exists(t, w, "v1", "ConfigMap", provisioningNS, "shop.old-page") {
		t.Fatal("a declared object was deleted")
	}
	// One Argo CD would prune goes; so does one it does not track at all.
	for _, name := range []string{"app-odoo", "app-notes"} {
		if res, err := w.svc.RemoveResidue(context.Background(), "Composition", name, "", "ada"); err != nil || res.Status != "deleted" {
			t.Fatalf("%s: %+v, %v", name, res, err)
		}
	}

	// No Application to ask.
	app := argoCatalogue()
	if err := w.objs.Delete(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	_, err = w.svc.RemoveResidue(context.Background(), "ConfigMap", "legacy.asset", "", "ada")
	if !errors.Is(err, ErrStillDeclared) || !strings.Contains(err.Error(), "not known") {
		t.Fatalf("no Application: %v", err)
	}
	if !exists(t, w, "v1", "ConfigMap", provisioningNS, "legacy.asset") {
		t.Fatal("deleted without Argo CD's word")
	}
}

// An unused profile is deleted only once Argo CD reports that the catalogue
// directory no longer declares it: until then it would be applied again.
func TestAnUnusedProfileGoesOnlyOnceArgoReportsItGoneFromTheDirectory(t *testing.T) {
	w := residueWorld(t)
	ctx := context.Background()

	// Still declared: the director has not committed, or Argo CD has not
	// synced.
	_, err := w.svc.RemoveResidue(ctx, "ComponentProfile", "notes", "", "ada")
	if !errors.Is(err, ErrStillDeclared) {
		t.Fatalf("declared profile: %v", err)
	}
	// Not tracked at all is not enough for a profile either.
	app := argoCatalogue()
	if err := w.objs.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {
		t.Fatal(err)
	}
	app.Object["status"] = map[string]any{"resources": []any{}}
	if err := w.objs.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.RemoveResidue(ctx, "ComponentProfile", "notes", "", "ada"); !errors.Is(err, ErrStillDeclared) {
		t.Fatalf("untracked profile: %v", err)
	}
	if !exists(t, w, "gentianos.io/v1alpha1", "ComponentProfile", "", "notes") {
		t.Fatal("the profile was deleted while still declared")
	}

	// Argo CD has synced the commit that removed it.
	app.Object["status"] = argoCatalogue("ComponentProfile/notes!", "ComponentProfile/shop").Object["status"]
	if err := w.objs.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	res, err := w.svc.RemoveResidue(ctx, "ComponentProfile", "notes", "", "ada")
	if err != nil || res.Status != "deleted" || res.Deleted.Class != ResidueUnusedProfile {
		t.Fatalf("answer = %+v, %v", res, err)
	}
	if exists(t, w, "gentianos.io/v1alpha1", "ComponentProfile", "", "notes") {
		t.Fatal("the profile is still there")
	}
	// The profile it shares the directory with is untouched.
	if !exists(t, w, "gentianos.io/v1alpha1", "ComponentProfile", "", "shop") {
		t.Fatal("another profile went with it")
	}
}

// The list is a read the usher's identity is admitted to; the removal is the
// director's, names the person, and answers a refusal with which it was.
func TestTheResidueRoutesAreAReadAndADirectorsCommand(t *testing.T) {
	w := residueWorld(t)
	_, auth := cluster()
	mux := (&HTTPServer{Auth: auth, Service: w.svc}).routes()
	call := func(method, path, token, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("X-Gentian-Actor", "ada@example.com")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	for _, token := range []string{"usher", "director"} {
		code, out := call("GET", "/v1/catalogue/residue", token, "")
		if list, _ := out["residue"].([]any); code != http.StatusOK || len(list) != 9 {
			t.Fatalf("the read as %s: %d %v", token, code, out)
		}
	}
	remove := "/v1/actions/remove-catalogue-residue"
	if code, _ := call("POST", remove, "usher", `{"kind":"ConfigMap","name":"shop.old-page"}`); code != http.StatusForbidden {
		t.Fatalf("the usher removed something: %d", code)
	}
	if code, _ := call("POST", remove, "", `{"kind":"ConfigMap","name":"shop.old-page"}`); code != http.StatusUnauthorized {
		t.Fatalf("nobody removed something: %d", code)
	}
	if !exists(t, w, "v1", "ConfigMap", provisioningNS, "shop.old-page") {
		t.Fatal("a refused caller deleted something")
	}

	if code, out := call("POST", remove, "director", `{"kind":"ConfigMap","name":"shop.theme"}`); code != http.StatusConflict ||
		out["reason"] != RefusedNotResidue || !strings.Contains(out["detail"].(string), "Nothing was deleted") {
		t.Fatalf("a live companion: %d %v", code, out)
	}
	if code, out := call("POST", remove, "director", `{"kind":"ComponentProfile","name":"notes"}`); code != http.StatusConflict ||
		out["reason"] != RefusedStillDeclared {
		t.Fatalf("a declared profile: %d %v", code, out)
	}
	for _, body := range []string{``, `{"kind":"Secret","name":"x"}`, `{"kind":"ConfigMap","name":"shop.old-page","names":["a","b"]}`, `{"kind":"ConfigMap"}`} {
		if code, _ := call("POST", remove, "director", body); code != http.StatusBadRequest {
			t.Errorf("body %q: %d", body, code)
		}
	}
	code, out := call("POST", remove, "director", `{"kind":"ConfigMap","name":"shop.old-page"}`)
	deleted, _ := out["deleted"].(map[string]any)
	if code != http.StatusOK || out["status"] != "deleted" || deleted["name"] != "shop.old-page" ||
		!strings.Contains(out["message"].(string), "ada@example.com") {
		t.Fatalf("the removal: %d %v", code, out)
	}
	if exists(t, w, "v1", "ConfigMap", provisioningNS, "shop.old-page") {
		t.Fatal("answered deleted, and it is there")
	}
}

// What the operator may delete of the catalogue's kinds is what the removal
// needs and no more. For a Composition that is get, list and delete -- it
// never writes one -- and nothing else of Crossplane's definitions.
func TestTheOperatorMayDeleteTheResidueKindsAndWritesNoComposition(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "gentian-os", "templates", "clusterrole.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, rules, ok := strings.Cut(string(raw), "\nrules:\n")
	if !ok {
		t.Fatal("the chart's ClusterRole has no rules")
	}
	var role struct {
		Rules []struct {
			APIGroups     []string `json:"apiGroups"`
			Resources     []string `json:"resources"`
			ResourceNames []string `json:"resourceNames"`
			Verbs         []string `json:"verbs"`
		} `json:"rules"`
	}
	if err := yaml.Unmarshal([]byte("rules:\n"+rules), &role); err != nil {
		t.Fatal(err)
	}
	verbs := map[string]map[string]bool{}
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				key := group + "/" + resource
				if verbs[key] == nil {
					verbs[key] = map[string]bool{}
				}
				for _, verb := range rule.Verbs {
					verbs[key][verb] = true
				}
			}
		}
	}
	held := func(key string) []string {
		var out []string
		for verb := range verbs[key] {
			out = append(out, verb)
		}
		sort.Strings(out)
		return out
	}

	// The four companion kinds, and the profile.
	for _, key := range []string{
		"apiextensions.crossplane.io/compositions", "gentianos.io/oidcpackcatalogs",
		"gentianos.io/customizations", "/configmaps", "gentianos.io/componentprofiles",
	} {
		if !verbs[key]["delete"] || !verbs[key]["list"] || !verbs[key]["get"] {
			t.Errorf("%s: %v, and the removal needs get, list and delete", key, held(key))
		}
	}
	// A Composition is read and deleted, never written; and it is the one
	// thing of Crossplane's definitions the operator touches.
	if got := held("apiextensions.crossplane.io/compositions"); !reflect.DeepEqual(got, []string{"delete", "get", "list"}) {
		t.Errorf("compositions: %v, want delete, get, list", got)
	}
	for key := range verbs {
		if strings.HasPrefix(key, "apiextensions.crossplane.io/") && key != "apiextensions.crossplane.io/compositions" {
			t.Errorf("the operator holds rights on %s: %v", key, held(key))
		}
	}
	// What the catalogue brings is not the operator's to write: Argo CD
	// applies it from git.
	for _, key := range []string{"gentianos.io/oidcpackcatalogs", "gentianos.io/componentprofiles"} {
		for _, verb := range []string{"create", "update", "patch", "*"} {
			if verbs[key][verb] {
				t.Errorf("the operator may %s %s", verb, key)
			}
		}
	}
	// The kinds of this API group the operator may delete, all of them: one
	// more is a decision, written down here.
	var deletable []string
	for key := range verbs {
		if group, resource, _ := strings.Cut(key, "/"); group == "gentianos.io" && verbs[key]["delete"] {
			deletable = append(deletable, resource)
		}
	}
	sort.Strings(deletable)
	want := []string{
		"appgrants", "apps", "backuppolicies", "componentprofiles", "components", "credentialrequirements",
		"customizations", "integrationbindings", "oidcpackcatalogs", "platformsecuritypolicies", "tenantexports",
		"tenantexportschedules", "tenantrestores", "tenants", "xtenants",
	}
	if !reflect.DeepEqual(deletable, want) {
		t.Errorf("the operator may delete %v of gentianos.io, want %v", deletable, want)
	}
}
