/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package tilecatalogue

import (
	"strings"
	"testing"
)

// What the operator writes is what the director reads, header and all: the
// explanation at the top of the ConfigMap is a comment, so it has to survive
// the round trip without becoming part of the data.
func TestTheHeaderIsAComment(t *testing.T) {
	rendered, err := Marshal(Catalogue{Tiles: []Tile{{
		Name: "headlamp", DisplayName: "Cluster", Icon: "cluster",
		URL: "https://headlamp.k.example/", Object: "cluster:demo",
		AnyOf: []string{"can_audit"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse([]byte(rendered))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tiles) != 1 || got.Tiles[0].URL != "https://headlamp.k.example/" {
		t.Fatalf("got %+v", got.Tiles)
	}
}

// A tile that cannot be answered costs that tile and not the page. Every one
// of these is a projection the operator should not have written, and the
// reader's job when it meets one is to keep serving the rest.
func TestAnUnanswerableTileIsDropped(t *testing.T) {
	complete := Tile{
		Name: "notes", DisplayName: "Notes", Icon: "notes",
		URL: "https://notes.k.example/", Object: "app:demo/notes",
		AnyOf: []string{"can_launch"},
	}
	cases := map[string]struct {
		change func(Tile) Tile
		kept   int
	}{
		"complete":             {func(t Tile) Tile { return t }, 1},
		"no name to show":      {func(t Tile) Tile { t.Name = ""; return t }, 0},
		"nowhere to lead":      {func(t Tile) Tile { t.URL = ""; return t }, 0},
		"nothing to ask about": {func(t Tile) Tile { t.Object = ""; return t }, 0},
		"no question to ask":   {func(t Tile) Tile { t.AnyOf = nil; return t }, 0},
	}
	for name, c := range cases {
		rendered, err := Marshal(Catalogue{Tiles: []Tile{c.change(complete)}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse([]byte(rendered))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(got.Tiles) != c.kept {
			t.Errorf("%s: kept %d, want %d", name, len(got.Tiles), c.kept)
		}
	}
}

// An empty catalogue is a list, not a null: the console renders a desktop with
// nothing on it, and a file that says "tiles: null" reads like a projection
// that failed halfway.
func TestAnEmptyCatalogueIsAList(t *testing.T) {
	rendered, err := Marshal(Catalogue{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse([]byte(rendered))
	if err != nil {
		t.Fatal(err)
	}
	if got.Tiles == nil || len(got.Tiles) != 0 {
		t.Fatalf("got %+v", got.Tiles)
	}
}

func TestURLDefaultsToTheFrontPage(t *testing.T) {
	cases := map[string]string{
		"":            "https://argocd.k.example/",
		"/auth/login": "https://argocd.k.example/auth/login",
		"auth/login":  "https://argocd.k.example/auth/login",
	}
	for path, want := range cases {
		if got := URL("argocd.k.example", path); got != want {
			t.Errorf("path %q: %s, want %s", path, got, want)
		}
	}
}

// A tile's translations survive the round trip through the projected file.
//
// They exist because thirty tiles in the catalogue are genuinely translated and
// the type that replaced the old one dropped the map. The projection is the
// narrow part of that path: if Marshal and Parse do not carry displayNames, the
// desktop has nothing to pick from however well it picks.
func TestTranslationsSurviveTheProjection(t *testing.T) {
	t.Parallel()
	body, err := Marshal(Catalogue{Tiles: []Tile{{
		Name: "files", DisplayName: "Files",
		DisplayNames: map[string]string{"de_DE": "Dateien"},
		Icon:         "files", URL: "https://files.example.test/",
		Object: "tenant:demo", AnyOf: []string{"can_view"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tiles) != 1 {
		t.Fatalf("tiles = %d", len(got.Tiles))
	}
	if got.Tiles[0].DisplayNames["de_DE"] != "Dateien" {
		t.Fatalf("translations lost: %+v", got.Tiles[0].DisplayNames)
	}
	// And a tile with none must not grow an empty map, which would render as
	// `displayNames: {}` in every projected file for no reason.
	body, err = Marshal(Catalogue{Tiles: []Tile{{
		Name: "plain", DisplayName: "Plain", Icon: "x", URL: "https://x.test/",
		Object: "tenant:demo", AnyOf: []string{"can_view"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "displayNames") {
		t.Fatalf("a tile with no translations carries the key:\n%s", body)
	}
}
