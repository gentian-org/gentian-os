/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/gentian-org/gentian-os/internal/hostnames"
)

// A tenant's manifest is edited as text, so that the comments in it and
// everything nobody asked to change survive. These are the manifests there
// are: the director's own, with its comments and an empty or a filled apps
// list; an import's, marshalled, with its lists at the key's own indentation;
// and ones that already carry a catalogue section in either form.

const directorWritten = `# Tenant demo, brought on through the director.
apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
  annotations:
    argocd.argoproj.io/sync-wave: "2"
spec:
  displayName: Demo
  isolation:
    mode: namespace
    keycloakRealm: demo
  # Retain, so retiring the tenant does not take its data with it.
  deletionPolicy: Retain
  quotas:
    maxApps: 20
  # Apps are installed through the director, which appends to this list.
  apps: []
`

const withApps = `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  displayName: Demo
  apps:
  - profile: nextcloud
    digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
    catalogue: gentian
    addons:
    - calendar
  - profile: element
`

const imported = `# Tenant demo, imported from a bundle through the director.
apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  annotations:
    argocd.argoproj.io/sync-wave: "2"
  name: demo
spec:
  apps:
  - profile: nextcloud
  deletionPolicy: Retain
  displayName: Demo
  isolation:
    keycloakRealm: demo
`

const importedWithCatalogue = `apiVersion: gentianos.io/v1alpha1
kind: Tenant
metadata:
  name: demo
spec:
  apps:
  - profile: nextcloud
  catalogue:
    delegated: true
    sources:
    - addedBy: cluster
      name: partner
      url: https://partner.example.com/apps
  displayName: Demo
`

const noTrailingNewline = "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: demo\nspec:\n  displayName: Demo\n  apps: []"

func mustParse(t *testing.T, text string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, text)
	}
	return doc
}

// withoutCatalogue is a manifest's spec with the catalogue section taken
// out, as YAML: what an edit must leave exactly as it was.
func withoutCatalogue(t *testing.T, text string) string {
	t.Helper()
	doc := mustParse(t, text)
	spec, _ := doc["spec"].(map[string]any)
	delete(spec, "catalogue")
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestATenantsCatalogueSectionIsWrittenIntoEveryManifestThereIs(t *testing.T) {
	want := TenantCatalogue{Delegated: true, Sources: []TenantCatalogueSource{
		{Name: "partner", URL: "https://partner.example.com/apps", AddedBy: "cluster"},
		{Name: "ours", URL: "https://demo.gitlab.io/catalogue", AddedBy: "tenant"},
	}}
	for name, manifest := range map[string]string{
		"the director's own":            directorWritten,
		"with apps":                     withApps,
		"an import":                     imported,
		"an import with a catalogue":    importedWithCatalogue,
		"no trailing newline":           noTrailingNewline,
		"an empty catalogue":            strings.Replace(directorWritten, "  apps: []\n", "  catalogue: {}\n  apps: []\n", 1),
		"a catalogue in the middle":     strings.Replace(directorWritten, "  deletionPolicy: Retain\n", "  catalogue:\n    delegated: false\n  deletionPolicy: Retain\n", 1),
		"a catalogue with a flow list":  strings.Replace(directorWritten, "  apps: []\n", "  catalogue:\n    sources: []\n  apps: []\n", 1),
		"a catalogue on one line":       strings.Replace(directorWritten, "  apps: []\n", "  catalogue: {delegated: true, sources: []}\n  apps: []\n", 1),
		"a catalogue followed by notes": strings.Replace(directorWritten, "  apps: []\n", "  catalogue:\n    delegated: true\n\n  # after\n  apps: []\n", 1),
	} {
		out, err := setTenantCatalogue(manifest, want)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		got, err := parseTenantCatalogue(out)
		if err != nil || got.Delegated != want.Delegated || len(got.Sources) != 2 || got.Sources[0] != want.Sources[0] || got.Sources[1] != want.Sources[1] {
			t.Errorf("%s: reads back as %+v (%v)\n%s", name, got, err, out)
		}
		if withoutCatalogue(t, out) != withoutCatalogue(t, manifest) {
			t.Errorf("%s: something else changed:\n%s", name, out)
		}
		// Every comment that was there is still there.
		for _, line := range strings.Split(manifest, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") && !strings.Contains(out, line) {
				t.Errorf("%s: the comment %q is gone", name, line)
			}
		}
		// Written again, nothing changes; taken away again, the section goes.
		again, err := setTenantCatalogue(out, want)
		if err != nil || again != out {
			t.Errorf("%s: writing the same again changed the text (%v)", name, err)
		}
		cleared, err := setTenantCatalogue(out, TenantCatalogue{})
		if err != nil {
			t.Errorf("%s: clearing: %v", name, err)
			continue
		}
		if strings.Contains(cleared, "\n  catalogue:") || withoutCatalogue(t, cleared) != withoutCatalogue(t, manifest) {
			t.Errorf("%s: cleared, it reads:\n%s", name, cleared)
		}
	}
}

// A manifest with no catalogue section reads as no sources and not
// delegated, and stays byte for byte as it is when nothing is to be said.
func TestAManifestWithoutTheSectionIsNotDelegatedAndIsLeftAlone(t *testing.T) {
	for _, manifest := range []string{directorWritten, withApps, imported, noTrailingNewline} {
		got, err := parseTenantCatalogue(manifest)
		if err != nil || got.Delegated || len(got.Sources) != 0 {
			t.Errorf("reads as %+v (%v)", got, err)
		}
		out, err := setTenantCatalogue(manifest, TenantCatalogue{})
		if err != nil || out != manifest {
			t.Errorf("an edit that says nothing changed the manifest (%v):\n%s", err, out)
		}
	}
}

// What is read is only what could be fetched from, and an entry that does
// not say who added it is the cluster's: the tenant's administrators do not
// get to remove what nobody is known to have given them.
func TestWhatIsReadOfATenantsCatalogueIsOnlyWhatIsUsable(t *testing.T) {
	manifest := strings.Replace(directorWritten, "  apps: []\n", `  catalogue:
    delegated: "yes"
    sources:
      - name: fine
        url: https://fine.example.com
      - name: clear
        url: http://clear.example.com
      - name: Not_A_Name
        url: https://x.example.com
      - name: fine
        url: https://second.example.com
      - name: odd
        url: https://odd.example.com
        addedBy: somebody
      - name: theirs
        url: https://theirs.example.com
        addedBy: tenant
  apps: []
`, 1)
	if _, err := parseTenantCatalogue(manifest); err == nil {
		t.Fatal("a delegation that is not true or false was read as one")
	}
	got, err := parseTenantCatalogue(strings.Replace(manifest, `delegated: "yes"`, "delegated: false", 1))
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, src := range got.Sources {
		said = append(said, src.Name+"="+src.AddedBy)
	}
	if strings.Join(said, " ") != "fine=cluster odd=cluster theirs=tenant" || got.Sources[0].URL != "https://fine.example.com" {
		t.Fatalf("read %v", got.Sources)
	}
}

// The Cluster claim's sources are rewritten in place: the App Store's
// address, the comments around the list and the commented example after it
// stay as the installer wrote them.
func TestTheClaimsSourcesAreRewrittenAndNothingElse(t *testing.T) {
	claim := `apiVersion: gentianos.io/v1alpha1
kind: Cluster
metadata:
  name: demo-cluster
spec:
  kernelDomain: k.example

  # Catalogues this cluster may fetch profiles from.
  catalogue:
    # The base address of the App Store API.
    storeUrl: https://store.example.com
    sources:
      - name: gentian
        url: https://gentian-org.github.io/gentian-apps
      # - name: in-house
      #   url: https://git.example.com/profiles
  mail:
    serviceMode: internal
`
	added, err := setClaimSources(claim, []CatalogueSource{
		{Name: "gentian", URL: "https://gentian-org.github.io/gentian-apps"},
		{Name: "partner", URL: "https://partner.example.com/apps"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(claim, "        url: https://gentian-org.github.io/gentian-apps\n",
		"        url: https://gentian-org.github.io/gentian-apps\n      - name: partner\n        url: https://partner.example.com/apps\n", 1)
	if added != want {
		t.Fatalf("after adding:\n%s", added)
	}
	removed, err := setClaimSources(added, []CatalogueSource{{Name: "gentian", URL: "https://gentian-org.github.io/gentian-apps"}})
	if err != nil || removed != claim {
		t.Fatalf("after removing again (%v):\n%s", err, removed)
	}
	none, err := setClaimSources(claim, nil)
	if err != nil || !strings.Contains(none, "    sources: []\n      # - name: in-house") || !strings.Contains(none, "storeUrl: https://store.example.com") {
		t.Fatalf("with none left (%v):\n%s", err, none)
	}

	// A claim that says nothing about catalogues, one that says `{}`, and
	// one that carries the section only as a comment: each gets the list.
	for name, bare := range map[string]string{
		"nothing":    "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  kernelDomain: k.example\n",
		"empty":      "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  catalogue: {}\n  kernelDomain: k.example\n",
		"commented":  "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  kernelDomain: k.example\n  # catalogue:\n  #   sources: []\n",
		"store only": "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  catalogue:\n    storeUrl: https://store.example.com\n  kernelDomain: k.example\n",
	} {
		out, err := setClaimSources(bare, []CatalogueSource{{Name: "partner", URL: "https://partner.example.com/apps"}})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		got, err := claimCatalogueSources(out)
		if err != nil || len(got) != 1 || got[0].Name != "partner" {
			t.Errorf("%s: reads back as %v (%v)\n%s", name, got, err, out)
		}
		spec, _ := mustParse(t, out)["spec"].(map[string]any)
		if spec["kernelDomain"] != "k.example" {
			t.Errorf("%s: the rest of the claim changed:\n%s", name, out)
		}
	}
	// A section written on one line with something in it is not rewritten
	// by guesswork.
	flow := "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c\nspec:\n  catalogue: {storeUrl: https://store.example.com}\n"
	if _, err := setClaimSources(flow, []CatalogueSource{{Name: "partner", URL: "https://partner.example.com/apps"}}); !errors.Is(err, ErrCatalogueFlowForm) {
		t.Fatalf("a one-line section: %v", err)
	}
}

// The platform's own profiles are not in the deployments repository, so the
// director knows their names from a list. The list is the chart's.
func TestThePlatformProfilesAreTheOnesTheChartShips(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	templates := filepath.Join(filepath.Dir(here), "..", "..", "..", "charts", "gentian-os", "templates")
	files, err := filepath.Glob(filepath.Join(templates, "componentprofile-*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no profile templates under %s (%v)", templates, err)
	}
	name := regexp.MustCompile(`(?m)^kind: ComponentProfile\nmetadata:\n  name: ([a-z0-9-]+)$`)
	var shipped []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		m := name.FindSubmatch(raw)
		if m == nil {
			t.Fatalf("%s names no ComponentProfile this test can read", file)
		}
		shipped = append(shipped, string(m[1]))
	}
	var listed []string
	for n := range platformProfiles {
		listed = append(listed, n)
	}
	sort.Strings(shipped)
	sort.Strings(listed)
	if strings.Join(shipped, ",") != strings.Join(listed, ",") {
		t.Fatalf("the chart ships %v and platformProfiles lists %v", shipped, listed)
	}
}

// Whose a profile is, is the director's to record. A served profile that
// says so itself is refused, and so is one that brings its own bundle.
func TestAProfileThatStatesItsOwnOriginIsRefused(t *testing.T) {
	for _, key := range []string{"gentianos.io/catalogue-origin", "gentianos.io/profile-bundle"} {
		body := "apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: element\n  annotations:\n    " + key + ": cluster/gentian\n"
		if _, err := renderBundle("element", []byte(body), "tenant/demo/ours"); !errors.Is(err, ErrProfileStatesOrigin) {
			t.Errorf("a profile stating %s: %v", key, err)
		}
	}
	patch, err := renderBundle("element", []byte("apiVersion: gentianos.io/v1alpha1\nkind: ComponentProfile\nmetadata:\n  name: element\n"), "tenant/demo/ours")
	if err != nil || !strings.Contains(string(patch), "    gentianos.io/catalogue-origin: tenant/demo/ours\n") {
		t.Fatalf("the patch (%v):\n%s", err, patch)
	}
}

// The rule for a name that is already a profile's, in one table.
func TestWhichOriginMayTakeAName(t *testing.T) {
	for _, c := range []struct {
		name, origin string
		have         MaterialisedProfile
		taken        bool
	}{
		{"app", "cluster/gentian", MaterialisedProfile{}, false},
		{"app", "tenant/demo/ours", MaterialisedProfile{}, false},
		{"app", "cluster/gentian", MaterialisedProfile{Present: true, Origin: "cluster/gentian"}, false},
		{"app", "tenant/demo/ours", MaterialisedProfile{Present: true, Origin: "tenant/demo/ours"}, false},
		{"app", "cluster/other", MaterialisedProfile{Present: true, Origin: "cluster/gentian"}, false},
		{"app", "cluster/gentian", MaterialisedProfile{Present: true}, false},
		{"app", "tenant/demo/ours", MaterialisedProfile{Present: true}, true},
		{"app", "tenant/demo/ours", MaterialisedProfile{Present: true, Origin: "cluster/gentian"}, true},
		{"app", "tenant/demo/ours", MaterialisedProfile{Present: true, Origin: "tenant/solo/ours"}, true},
		{"app", "tenant/demo/ours", MaterialisedProfile{Present: true, Origin: "tenant/demo/other"}, true},
		{"app", "cluster/gentian", MaterialisedProfile{Present: true, Origin: "tenant/demo/ours"}, true},
		{"app", "cluster/gentian", MaterialisedProfile{Present: true, Origin: "nonsense"}, true},
		{"desktop", "cluster/gentian", MaterialisedProfile{}, true},
		{"admin-console", "tenant/demo/ours", MaterialisedProfile{}, true},
	} {
		err := nameTaken(c.name, c.origin, c.have)
		var taken *ErrProfileNameTaken
		if errors.As(err, &taken) != c.taken {
			t.Errorf("%s from %s over %+v: %v, want taken=%v", c.name, c.origin, c.have, err, c.taken)
		}
	}
}

// A reserved address name is held by a profile the platform ships, and by
// nothing a catalogue can bring: every owner on the list is a name the
// director refuses to materialise from any catalogue.
func TestEveryHolderOfAReservedAddressIsAPlatformProfile(t *testing.T) {
	holders := 0
	for _, r := range hostnames.PlatformLabels() {
		if r.Owner == "" {
			continue
		}
		holders++
		if !PlatformProfile(r.Owner) {
			t.Errorf("%s is held by %s, which is not a profile the platform ships: a catalogue could publish it", r.Label, r.Owner)
		}
		for _, origin := range []string{"cluster/main", "tenant/acme/ours"} {
			var taken *ErrProfileNameTaken
			if err := nameTaken(r.Owner, origin, MaterialisedProfile{}); !errors.As(err, &taken) || !taken.Platform {
				t.Errorf("a profile named %s from %s would be materialised: %v", r.Owner, origin, err)
			}
		}
	}
	if holders == 0 {
		t.Fatal("no reserved address name has a holder")
	}
}
