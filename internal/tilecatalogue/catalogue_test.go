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

package tilecatalogue

import "testing"

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
