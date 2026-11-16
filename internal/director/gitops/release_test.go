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

const releaseClaim = `apiVersion: gentianos.io/v1alpha1
kind: Cluster
metadata:
  name: demo
spec:
  kernelDomain: example.org
  catalogue:
    storeUrl: https://store.example.com
    sources:
      - name: gentian
        # The development catalog: this cluster was installed from develop.
        url: https://gentian-org.github.io/gentian-apps/develop
      - name: in-house
        url: https://git.example.com/profiles
  oidc:
    discoveryUrl: https://id.example.org/auth/realms/kernel
`

func parsedRelease(t *testing.T, text string) (ReleaseSpec, []CatalogueSource) {
	t.Helper()
	var claim struct {
		Spec struct {
			Release ReleaseSpec `json:"release"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &claim); err != nil {
		t.Fatalf("the edited claim is not YAML: %v\n%s", err, text)
	}
	sources, err := claimCatalogueSources(text)
	if err != nil {
		t.Fatal(err)
	}
	return claim.Spec.Release, sources
}

func TestSettingAVersionWritesTheBlockAndMovesTheCatalogue(t *testing.T) {
	out, changed, err := releaseEdit(releaseClaim, ReleaseSpec{Version: "v0.5.1", AutoUpgrade: "patch"})
	if err != nil || !changed {
		t.Fatalf("edit: %v, changed %v", err, changed)
	}
	rel, sources := parsedRelease(t, out)
	if rel != (ReleaseSpec{Version: "v0.5.1", AutoUpgrade: "patch"}) {
		t.Fatalf("release = %+v", rel)
	}
	if sources[0].URL != ReleasedCatalogueURL || sources[1].URL != "https://git.example.com/profiles" {
		t.Fatalf("sources = %+v", sources)
	}
	if !strings.Contains(out, "  oidc:\n    discoveryUrl:") || !strings.HasSuffix(out, "\n") {
		t.Fatalf("the rest of the claim must stand, and the file end in a newline:\n%s", out)
	}
	// The same again is no change.
	if _, changed, err := releaseEdit(out, ReleaseSpec{Version: "v0.5.1", AutoUpgrade: "patch"}); err != nil || changed {
		t.Fatalf("second edit: %v, changed %v", err, changed)
	}
	// A track replaces the version, drops the mode and points at the
	// development catalog again.
	out2, changed, err := releaseEdit(out, ReleaseSpec{Track: "develop"})
	if err != nil || !changed {
		t.Fatalf("track edit: %v, changed %v", err, changed)
	}
	rel, sources = parsedRelease(t, out2)
	if rel != (ReleaseSpec{Track: "develop"}) || sources[0].URL != DevelopmentCatalogueURL {
		t.Fatalf("after the track: %+v %+v", rel, sources)
	}
	if strings.Count(out2, "release:") != 1 || strings.Contains(out2, "autoUpgrade") {
		t.Fatalf("the block must be replaced whole:\n%s", out2)
	}
}

func TestAClaimWithoutAGentianSourceGetsTheBlockAlone(t *testing.T) {
	claim := "apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: demo\nspec:\n  kernelDomain: example.org\n"
	out, changed, err := releaseEdit(claim, ReleaseSpec{Version: "v0.5.0"})
	if err != nil || !changed {
		t.Fatalf("edit: %v, changed %v", err, changed)
	}
	rel, sources := parsedRelease(t, out)
	if rel.Version != "v0.5.0" || len(sources) != 0 {
		t.Fatalf("release = %+v sources = %+v", rel, sources)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("the file must still end in a newline")
	}
}

func TestCheckReleaseHoldsTheSchemasRules(t *testing.T) {
	bad := []ReleaseSpec{
		{},
		{Track: "develop", Version: "v0.5.1"},
		{Version: "0.5.1"},
		{Version: "v0.5"},
		{Track: "a branch"},
		{Track: "develop", AutoUpgrade: "patch"},
		{Version: "v0.5.1", AutoUpgrade: "major"},
	}
	for _, r := range bad {
		if err := CheckRelease(r); err == nil {
			t.Errorf("CheckRelease(%+v) passed", r)
		}
	}
	good := []ReleaseSpec{{Track: "develop"}, {Track: "feat/942-x"}, {Version: "v0.5.1"}, {Version: "v0.6.0-rc.1", AutoUpgrade: "minor"}, {Version: "v0.5.1", AutoUpgrade: "none"}}
	for _, r := range good {
		if err := CheckRelease(r); err != nil {
			t.Errorf("CheckRelease(%+v) = %v", r, err)
		}
	}
	if CatalogueFor(ReleaseSpec{Version: "v0.5.1"}) != ReleasedCatalogueURL || CatalogueFor(ReleaseSpec{Track: "develop"}) != DevelopmentCatalogueURL {
		t.Fatal("CatalogueFor")
	}
}
