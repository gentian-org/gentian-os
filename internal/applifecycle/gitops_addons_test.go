/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package applifecycle

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
		[]string{"nextcloud-mail-ce", "nextcloud-calendar-ce"})
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
	once, _ := rewriteAddons(baseTenant, "nextcloud-base-ce", []string{"nextcloud-mail-ce"})
	twice, _ := rewriteAddons(once, "nextcloud-base-ce", []string{"nextcloud-deck-ce"})
	doc := parseTenant(t, twice)
	if len(doc.Spec.Apps[0].Addons) != 1 || doc.Spec.Apps[0].Addons[0] != "nextcloud-deck-ce" {
		t.Fatalf("addons: %+v", doc.Spec.Apps[0].Addons)
	}
	if n := strings.Count(twice, "addons:"); n != 1 {
		t.Fatalf("expected exactly one addons key, got %d:\n%s", n, twice)
	}
}

func TestRewriteAddonsEmptySelectionRemovesBlock(t *testing.T) {
	once, _ := rewriteAddons(baseTenant, "nextcloud-base-ce", []string{"nextcloud-mail-ce"})
	cleared, _ := rewriteAddons(once, "nextcloud-base-ce", nil)
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
	out, ok := rewriteAddons(src, "nextcloud-base-ce", []string{"nextcloud-deck-ce"})
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
	out, ok := rewriteAddons(src, "nextcloud-base-ce", []string{"nextcloud-mail-ce"})
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
	out, ok := rewriteAddons(src, "xwiki-ce", []string{"xwiki-extra-ce"})
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
	if _, ok := rewriteAddons(baseTenant, "not-installed-ce", []string{"x"}); ok {
		t.Fatal("expected not-found for an uninstalled profile")
	}
}

func TestRewriteAddonsDoesNotPartiallyMatchProfileName(t *testing.T) {
	src := strings.Replace(baseTenant,
		"- profile: nextcloud-base-ce", "- profile: nextcloud-base-ce-extra", 1)
	if _, ok := rewriteAddons(src, "nextcloud-base-ce", []string{"x"}); ok {
		t.Fatal("nextcloud-base-ce must not match nextcloud-base-ce-extra")
	}
}
