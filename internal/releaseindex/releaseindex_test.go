/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package releaseindex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const sample = `
releases:
  - version: v0.5.0
    line: "0.5"
    upgradesFrom: []
    ui: {desktop: 0.1.0-v0.5.0}
    catalogue: https://gentian-org.github.io/gentian-apps
  - version: v0.4.2
    line: "0.4"
    notes: https://example.org/v0.4.2
    upgradesFrom: ["0.4"]
  - version: v0.5.1
    line: "0.5"
    upgradesFrom: ["0.5"]
  - version: v0.6.0
    line: "0.6"
    upgradesFrom: ["0.5"]
  - version: v0.7.0
    line: "0.7"
    upgradesFrom: ["0.6"]
  - version: v0.7.1-rc.1
    line: "0.7"
    upgradesFrom: ["0.7"]
`

func TestTheIndexIsSortedAndChecked(t *testing.T) {
	idx, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.Releases[0].Version; got != "v0.4.2" {
		t.Fatalf("oldest = %s", got)
	}
	newest, _ := idx.Newest()
	if newest.Version != "v0.7.0" {
		t.Fatalf("newest = %s, a prerelease must not count", newest.Version)
	}
	if r, ok := idx.Find("0.5.0"); !ok || r.UI["desktop"] != "0.1.0-v0.5.0" {
		t.Fatalf("Find(0.5.0) = %v %v", r, ok)
	}
	for _, bad := range []string{
		"releases: [{version: v0.5.0, line: \"0.4\"}]",
		"releases: [{version: v0.5.0}, {version: 0.5.0}]",
		"releases: [{version: five}]",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) passed", bad)
		}
	}
}

func TestTheCommittedIndexParses(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("..", "..", "docs", "releases", "index.yaml"))
	if err != nil {
		t.Skip(err)
	}
	idx, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := idx.Find("v0.5.0"); !ok || r.UI["adminConsole"] != "0.1.1-v0.5.0" || r.Catalogue == "" {
		t.Fatalf("v0.5.0 = %+v", r)
	}
}

func TestWhatAModeTakes(t *testing.T) {
	idx, _ := Parse([]byte(sample))
	cases := []struct {
		current string
		mode    Mode
		take    string
		pending string
	}{
		{"0.5.0", ModeNone, "", "v0.7.0"},
		{"0.5.0", ModePatch, "v0.5.1", "v0.7.0"},
		{"0.5.0", ModeMinor, "v0.6.0", "v0.7.0"}, // one line at a time: v0.7.0 is in place from 0.6 only
		{"0.6.0", ModeMinor, "v0.7.0", ""},
		{"0.4.2", ModeMinor, "", "v0.7.0"}, // v0.5.0 is a fresh install
		{"0.4.2", ModePatch, "", "v0.7.0"},
		{"0.7.0", ModeMinor, "", ""}, // the rc is not a release
		{"0.5.1", ModePatch, "", "v0.7.0"},
		{"nonsense", ModePatch, "", ""},
	}
	for _, c := range cases {
		take, pending := idx.Next(c.current, c.mode)
		got := func(r *Release) string {
			if r == nil {
				return ""
			}
			return r.Version
		}
		if got(take) != c.take || got(pending) != c.pending {
			t.Errorf("Next(%s, %s) = take %q pending %q, want %q %q", c.current, c.mode, got(take), got(pending), c.take, c.pending)
		}
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()
	idx, err := Fetch(context.Background(), srv.Client(), srv.URL+"/index.yaml")
	if err != nil || len(idx.Releases) != 6 {
		t.Fatalf("Fetch: %v, %d releases", err, len(idx.Releases))
	}
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL+"/missing"); err == nil {
		t.Fatal("a 404 must be an error")
	}
}
