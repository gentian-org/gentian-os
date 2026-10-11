/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package branding

import (
	_ "embed"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The default brand is itself a valid document, and renders the scale every
// page reads its primary colour from.
func TestTheDefaultBrandRenders(t *testing.T) {
	tokens, err := Parse(defaultTokens)
	if err != nil {
		t.Fatal(err)
	}
	css := CSS(tokens)
	for _, want := range []string{
		"--brand-color-brand-500: #262696;",
		"--brand-color-paper-0: #F4F1EA;",
		`--brand-font-family-sans: "Hanken Grotesk", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;`,
		"--brand-font-family-display: var(--brand-font-family-sans);",
		"--brand-radius-2: 10px;",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("missing %s in\n%s", want, css)
		}
	}
}

// Colours in the 2025.10 object form render in their own space; the hex
// fallback is used only where it says the same thing.
func TestColoursRenderInTheirOwnSpace(t *testing.T) {
	tokens, err := Parse([]byte(`{"c": {"$type": "color",
		"p3":  {"$value": {"colorSpace": "display-p3", "components": [1, 0.5, 0]}},
		"ok":  {"$value": {"colorSpace": "oklch", "components": [0.6, 0.2, 260], "alpha": 0.5}},
		"rgb": {"$value": {"colorSpace": "srgb", "components": [1, 0, 0], "alpha": 0.5, "hex": "#ff0000"}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tk := range tokens {
		got[tk.Property()] = tk.CSS
	}
	if got["--brand-c-p3"] != "color(display-p3 1 0.5 0)" || got["--brand-c-ok"] != "oklch(0.6 0.2 260 / 0.5)" || got["--brand-c-rgb"] != "rgb(255 0 0 / 0.5)" {
		t.Fatalf("got %v", got)
	}
}

// What would escape the declaration, or fetch something, is refused rather
// than emitted: a brand is written by an administrator and read by every
// page on the cluster.
func TestAValueThatWouldEscapeItsDeclarationIsRefused(t *testing.T) {
	for _, doc := range []string{
		`{"c": {"$type": "color", "x": {"$value": "red; } body { display: none"}}}`,
		`{"c": {"$type": "color", "x": {"$value": "url(https://evil.example/x)"}}}`,
		`{"f": {"$type": "fontFamily", "x": {"$value": "A\"; } *{x:"}}}`,
		`{"c": {"x": {"$value": "#fff"}}}`,
		`{"c": {"$type": "gradient", "x": {"$value": []}}}`,
		`{"bad name": {"$type": "color", "x": {"$value": "#fff"}}}`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("admitted %s", doc)
		}
	}
}

// A brand that sets only its primary colour keeps the rest of the default
// look, and its name reaches the identity and the installed app.
func TestABrandOverridesOnlyWhatItSets(t *testing.T) {
	spec := &gentianov1alpha1.BrandingSpec{
		Identity: gentianov1alpha1.BrandIdentity{Name: "Acme Cloud"},
		Tokens:   &runtime.RawExtension{Raw: []byte(`{"color": {"$type": "color", "brand": {"500": {"$value": "#c0392b"}}}}`)},
	}
	rendered, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	files := rendered.Text
	css := files[CSSFile]
	if !strings.Contains(css, "--brand-color-brand-500: #c0392b;") || !strings.Contains(css, "--brand-color-paper-0: #F4F1EA;") {
		t.Fatalf("css:\n%s", css)
	}
	if !strings.Contains(files[IdentityFile], `"name":"Acme Cloud","shortName":"Acme Cloud"`) {
		t.Fatalf("identity: %s", files[IdentityFile])
	}
	if !strings.Contains(files[ManifestFile], `"theme_color": "#c0392b"`) || !strings.Contains(files[ManifestFile], `"name": "Acme Cloud"`) {
		t.Fatalf("manifest: %s", files[ManifestFile])
	}
	if files, err := Render(nil); err != nil || !strings.Contains(files.Text[IdentityFile], `"name":"Gentian"`) {
		t.Fatalf("no branding: %v %v", files, err)
	}
}

// An icon given as a data URL is published as a file of its own, and both
// brand.json and the manifest name it by its relative URL.
func TestADataURLIconBecomesAFile(t *testing.T) {
	spec := &gentianov1alpha1.BrandingSpec{Identity: gentianov1alpha1.BrandIdentity{Icons: []gentianov1alpha1.BrandIcon{
		{Src: "data:image/svg+xml;base64,PHN2Zy8+", Type: "image/svg+xml", Purpose: "any"},
		{Src: "https://cdn.example/logo.png"},
	}}}
	files, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(files.Binary["icon-0.svg"]) != "<svg/>" {
		t.Fatalf("binary = %v", files.Binary)
	}
	for _, f := range []string{IdentityFile, ManifestFile} {
		if !strings.Contains(files.Text[f], `"icon-0.svg"`) || strings.Contains(files.Text[f], "base64") || !strings.Contains(files.Text[f], "https://cdn.example/logo.png") {
			t.Fatalf("%s: %s", f, files.Text[f])
		}
	}
}
