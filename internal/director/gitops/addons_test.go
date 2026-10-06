/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// This edits a GitOps file that ArgoCD applies to a live cluster, so every case
// asserts the whole document still parses to the expected structure — not merely
// that the addons line appears somewhere.

const baseTenant = `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  displayName: Demo
  apps:
  - profile: nextcloud-base-ce
  - profile: xwiki-ce
`

type tenantDoc struct {
	Spec struct {
		Apps []struct {
			Profile string   `json:"profile"`
			Addons  []string `json:"addons"`
			Config  *struct {
				Replicas int `json:"replicas"`
			} `json:"config"`
		} `json:"apps"`
		Quotas map[string]string `json:"quotas"`
	} `json:"spec"`
}

func parseTenant(t *testing.T, text string) tenantDoc {
	t.Helper()
	var doc tenantDoc
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("result is not valid YAML: %v\n---\n%s", err, text)
	}
	return doc
}

func TestRewriteAddonsAddsBlockToBareEntry(t *testing.T) {
	out, ok := rewriteAddons(baseTenant, "nextcloud-base-ce",
		[]string{"nextcloud-mail-ce", "nextcloud-calendar-ce"}, nil)
	if !ok {
		t.Fatal("expected profile to be found")
	}
	doc := parseTenant(t, out)
	got := doc.Spec.Apps[0]
	if got.Profile != "nextcloud-base-ce" || len(got.Addons) != 2 ||
		got.Addons[0] != "nextcloud-mail-ce" || got.Addons[1] != "nextcloud-calendar-ce" {
		t.Fatalf("entry: %+v", got)
	}
	if doc.Spec.Apps[1].Profile != "xwiki-ce" || len(doc.Spec.Apps[1].Addons) != 0 {
		t.Fatalf("sibling entry was modified: %+v", doc.Spec.Apps[1])
	}
}

func TestRewriteAddonsReplacesRatherThanAccumulates(t *testing.T) {
	once, _ := rewriteAddons(baseTenant, "nextcloud-base-ce", []string{"nextcloud-mail-ce"}, nil)
	twice, _ := rewriteAddons(once, "nextcloud-base-ce", []string{"nextcloud-deck-ce"}, nil)
	doc := parseTenant(t, twice)
	if len(doc.Spec.Apps[0].Addons) != 1 || doc.Spec.Apps[0].Addons[0] != "nextcloud-deck-ce" {
		t.Fatalf("addons: %+v", doc.Spec.Apps[0].Addons)
	}
	if n := strings.Count(twice, "addons:"); n != 1 {
		t.Fatalf("expected exactly one addons key, got %d:\n%s", n, twice)
	}
}

func TestRewriteAddonsEmptySelectionRemovesBlock(t *testing.T) {
	once, _ := rewriteAddons(baseTenant, "nextcloud-base-ce", []string{"nextcloud-mail-ce"}, nil)
	cleared, _ := rewriteAddons(once, "nextcloud-base-ce", nil, nil)
	if strings.Contains(cleared, "addons:") {
		t.Fatalf("addons block survived an empty selection:\n%s", cleared)
	}
	doc := parseTenant(t, cleared)
	if doc.Spec.Apps[0].Profile != "nextcloud-base-ce" || len(doc.Spec.Apps[0].Addons) != 0 {
		t.Fatalf("entry: %+v", doc.Spec.Apps[0])
	}
}

func TestRewriteAddonsPreservesOtherKeysAndComments(t *testing.T) {
	src := `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  apps:
  # keep this comment
  - profile: nextcloud-base-ce
    config:
      replicas: 2
    addons:
    - nextcloud-mail-ce
  - profile: xwiki-ce
`
	out, ok := rewriteAddons(src, "nextcloud-base-ce", []string{"nextcloud-deck-ce"}, nil)
	if !ok {
		t.Fatal("expected profile to be found")
	}
	doc := parseTenant(t, out)
	if doc.Spec.Apps[0].Config == nil || doc.Spec.Apps[0].Config.Replicas != 2 {
		t.Fatalf("config key was lost: %+v", doc.Spec.Apps[0])
	}
	if len(doc.Spec.Apps[0].Addons) != 1 || doc.Spec.Apps[0].Addons[0] != "nextcloud-deck-ce" {
		t.Fatalf("addons: %+v", doc.Spec.Apps[0].Addons)
	}
	if doc.Spec.Apps[1].Profile != "xwiki-ce" {
		t.Fatalf("sibling lost: %+v", doc.Spec.Apps)
	}
	if !strings.Contains(out, "# keep this comment") {
		t.Fatal("comment was dropped")
	}
}

func TestRewriteAddonsLastEntryInFile(t *testing.T) {
	src := strings.Replace(baseTenant, "  - profile: xwiki-ce\n", "", 1)
	out, ok := rewriteAddons(src, "nextcloud-base-ce", []string{"nextcloud-mail-ce"}, nil)
	if !ok {
		t.Fatal("expected profile to be found")
	}
	doc := parseTenant(t, out)
	if len(doc.Spec.Apps[0].Addons) != 1 {
		t.Fatalf("addons: %+v", doc.Spec.Apps[0])
	}
}

func TestRewriteAddonsStopsAtNextSection(t *testing.T) {
	src := baseTenant + "  quotas:\n    storage: 10Gi\n"
	out, ok := rewriteAddons(src, "xwiki-ce", []string{"xwiki-extra-ce"}, nil)
	if !ok {
		t.Fatal("expected profile to be found")
	}
	doc := parseTenant(t, out)
	if doc.Spec.Quotas["storage"] != "10Gi" {
		t.Fatalf("quotas section was damaged: %+v", doc.Spec.Quotas)
	}
	if len(doc.Spec.Apps[1].Addons) != 1 || doc.Spec.Apps[1].Addons[0] != "xwiki-extra-ce" {
		t.Fatalf("addons: %+v", doc.Spec.Apps[1])
	}
}

func TestRewriteAddonsUnknownProfileIsNotFound(t *testing.T) {
	if _, ok := rewriteAddons(baseTenant, "not-installed-ce", []string{"x"}, nil); ok {
		t.Fatal("expected not-found for an uninstalled profile")
	}
}

func TestRewriteAddonsDoesNotPartiallyMatchProfileName(t *testing.T) {
	src := strings.Replace(baseTenant,
		"- profile: nextcloud-base-ce", "- profile: nextcloud-base-ce-extra", 1)
	if _, ok := rewriteAddons(src, "nextcloud-base-ce", []string{"x"}, nil); ok {
		t.Fatal("nextcloud-base-ce must not match nextcloud-base-ce-extra")
	}
}

// Uninstall must take the whole entry. Leaving nested keys behind produced YAML
// that did not parse, and every reconcile after the uninstall failed with
// "did not find expected key" until the file was repaired by hand.

func TestRemoveAppEntryTakesNestedKeysWithIt(t *testing.T) {
	t.Parallel()
	src := `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  apps:
  - profile: odoo-base-ce
    addons:
    - odoo-crm-ce
    - odoo-accounting-ce
  - profile: nextcloud-base-ce
    addons:
    - nextcloud-mail-ce
`
	out, ok := removeAppEntry(src, "odoo-base-ce")
	if !ok {
		t.Fatal("expected the entry to be found")
	}
	if strings.Contains(out, "odoo-crm-ce") {
		t.Fatalf("orphaned addon survived:\n%s", out)
	}
	doc := parseTenant(t, out) // fails the test if the result does not parse
	if len(doc.Spec.Apps) != 1 || doc.Spec.Apps[0].Profile != "nextcloud-base-ce" {
		t.Fatalf("apps: %+v", doc.Spec.Apps)
	}
	if len(doc.Spec.Apps[0].Addons) != 1 {
		t.Fatalf("sibling addons damaged: %+v", doc.Spec.Apps[0])
	}
}

func TestRemoveAppEntryHandlesOtherNestedKeys(t *testing.T) {
	t.Parallel()
	src := `spec:
  apps:
  - profile: a-ce
    config:
      replicas: 2
    addons:
    - x-ce
  - profile: b-ce
  quotas:
    storage: 10Gi
`
	out, _ := removeAppEntry(src, "a-ce")
	doc := parseTenant(t, out)
	if len(doc.Spec.Apps) != 1 || doc.Spec.Apps[0].Profile != "b-ce" {
		t.Fatalf("apps: %+v", doc.Spec.Apps)
	}
	if doc.Spec.Quotas["storage"] != "10Gi" {
		t.Fatalf("following section damaged: %+v", doc.Spec.Quotas)
	}
}

func TestRemoveAppEntryBareEntryAndLastEntry(t *testing.T) {
	t.Parallel()
	out, ok := removeAppEntry(baseTenant, "xwiki-ce") // bare, and last in the file
	if !ok {
		t.Fatal("expected the entry to be found")
	}
	doc := parseTenant(t, out)
	if len(doc.Spec.Apps) != 1 || doc.Spec.Apps[0].Profile != "nextcloud-base-ce" {
		t.Fatalf("apps: %+v", doc.Spec.Apps)
	}
}

func TestRemoveAppEntryUnknownProfile(t *testing.T) {
	t.Parallel()
	if _, ok := removeAppEntry(baseTenant, "not-installed-ce"); ok {
		t.Fatal("expected not-found for an uninstalled profile")
	}
}

// pinnedDoc reads the entries with their pins, the way the operator's schema
// does.
type pinnedDoc struct {
	Spec struct {
		Apps []App `json:"apps"`
	} `json:"spec"`
}

func parsePinned(t *testing.T, text string) pinnedDoc {
	t.Helper()
	var doc pinnedDoc
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("result is not valid YAML: %v\n---\n%s", err, text)
	}
	return doc
}

var (
	pinA = "sha256:" + strings.Repeat("ab", 32)
	pinB = "sha256:" + strings.Repeat("cd", 32)
)

// A pinned addon is written beside the list, keyed by its name. The list
// itself stays a list of names.
func TestRewriteAddonsWritesPinsBesideTheList(t *testing.T) {
	out, ok := rewriteAddons(baseTenant, "nextcloud-base-ce",
		[]string{"nextcloud-mail-ce", "nextcloud-talk-ce"},
		[]AddonPin{{Name: "nextcloud-talk-ce", Digest: pinA, Catalogue: "main"}})
	if !ok {
		t.Fatal("expected profile to be found")
	}
	want := "  - profile: nextcloud-base-ce\n" +
		"    addons:\n    - nextcloud-mail-ce\n    - nextcloud-talk-ce\n" +
		"    addonPins:\n    - name: nextcloud-talk-ce\n      digest: " + pinA + "\n      catalogue: main\n" +
		"  - profile: xwiki-ce\n"
	if !strings.Contains(out, want) {
		t.Fatalf("the entry is not written as expected:\n%s", out)
	}
	doc := parsePinned(t, out)
	app := doc.Spec.Apps[0]
	if strings.Join(app.Addons, ",") != "nextcloud-mail-ce,nextcloud-talk-ce" || len(app.AddonPins) != 1 ||
		app.AddonPins[0] != (AddonPin{Name: "nextcloud-talk-ce", Digest: pinA, Catalogue: "main"}) {
		t.Fatalf("entry = %+v", app)
	}
	if len(doc.Spec.Apps) != 2 || doc.Spec.Apps[1].Profile != "xwiki-ce" || len(doc.Spec.Apps[1].AddonPins) != 0 {
		t.Fatalf("the sibling entry was damaged: %+v", doc.Spec.Apps)
	}
}

// Both blocks are replaced, not accumulated, and removed when there is
// nothing to write -- in whichever order and form they were in.
func TestRewriteAddonsReplacesPins(t *testing.T) {
	once, _ := rewriteAddons(baseTenant, "nextcloud-base-ce", []string{"a-ce", "b-ce"},
		[]AddonPin{{Name: "a-ce", Digest: pinA, Catalogue: "main"}, {Name: "b-ce", Digest: pinB, Catalogue: "main"}})
	twice, _ := rewriteAddons(once, "nextcloud-base-ce", []string{"b-ce"},
		[]AddonPin{{Name: "b-ce", Digest: pinA, Catalogue: "main"}})
	if strings.Count(twice, "addonPins:") != 1 || strings.Count(twice, "addons:") != 1 || strings.Contains(twice, pinB) {
		t.Fatalf("the blocks accumulated:\n%s", twice)
	}
	app := parsePinned(t, twice).Spec.Apps[0]
	if len(app.AddonPins) != 1 || app.AddonPins[0].Digest != pinA || strings.Join(app.Addons, ",") != "b-ce" {
		t.Fatalf("entry = %+v", app)
	}
	cleared, _ := rewriteAddons(twice, "nextcloud-base-ce", nil, nil)
	if cleared != baseTenant {
		t.Fatalf("clearing did not return the entry to what it was:\n%s", cleared)
	}

	// Written by a person: pins first, deeper sequence indent, a comment,
	// and other keys between and after.
	src := `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  apps:
    - profile: nextcloud-base-ce
      digest: ` + pinB + `
      addonPins:   # from the store
        - name: a-ce
          digest: ` + pinB + `
          catalogue: main
      config:
        replicas: 2
      addons:
        - a-ce
      defaultGrant: true
    - profile: xwiki-ce
  quotas:
    storage: 10Gi
`
	out, ok := rewriteAddons(src, "nextcloud-base-ce", []string{"c-ce"}, []AddonPin{{Name: "c-ce", Digest: pinA, Catalogue: "in-house"}})
	if !ok {
		t.Fatal("expected profile to be found")
	}
	if strings.Contains(out, "a-ce") || strings.Count(out, "addonPins:") != 1 {
		t.Fatalf("the old blocks were left behind:\n%s", out)
	}
	var doc struct {
		Spec struct {
			Apps []struct {
				App
				Config *struct {
					Replicas int `json:"replicas"`
				} `json:"config"`
			} `json:"apps"`
			Quotas map[string]string `json:"quotas"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("result is not valid YAML: %v\n---\n%s", err, out)
	}
	first := doc.Spec.Apps[0]
	if first.Digest != pinB || !first.DefaultGrant || first.Config == nil || first.Config.Replicas != 2 ||
		strings.Join(first.Addons, ",") != "c-ce" || len(first.AddonPins) != 1 || first.AddonPins[0].Catalogue != "in-house" {
		t.Fatalf("entry = %+v", first)
	}
	if len(doc.Spec.Apps) != 2 || doc.Spec.Quotas["storage"] != "10Gi" {
		t.Fatalf("what follows the entry was damaged:\n%s", out)
	}
}

// A list a person wrote on one line is the list all the same: it is replaced,
// not joined by a second key of the same name.
func TestRewriteAddonsReplacesAListWrittenOnOneLine(t *testing.T) {
	for _, flow := range []string{"addons: []", "addons: [a-ce, b-ce]", "addons: [a-ce]  # for now"} {
		src := strings.Replace(baseTenant, "  - profile: nextcloud-base-ce\n",
			"  - profile: nextcloud-base-ce\n    "+flow+"\n    addonPins: []\n", 1)
		parsePinned(t, src)
		out, ok := rewriteAddons(src, "nextcloud-base-ce", []string{"c-ce"}, []AddonPin{{Name: "c-ce", Digest: pinA}})
		if !ok {
			t.Fatal("expected profile to be found")
		}
		if strings.Count(out, "addons:") != 1 || strings.Count(out, "addonPins:") != 1 {
			t.Fatalf("%q was left beside the new block:\n%s", flow, out)
		}
		app := parsePinned(t, out).Spec.Apps[0]
		if strings.Join(app.Addons, ",") != "c-ce" || len(app.AddonPins) != 1 || app.AddonPins[0].Catalogue != "" {
			t.Fatalf("%q: entry = %+v", flow, app)
		}
	}
}
