/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package tiles

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"
)

func TestResolveIconKnown(t *testing.T) {
	uri, err := ResolveIcon("mail")
	if err != nil {
		t.Fatalf("ResolveIcon: %v", err)
	}
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("unexpected uri prefix: %s", uri[:32])
	}
}

func TestResolveIconUnknown(t *testing.T) {
	if _, err := ResolveIcon("not-a-real-icon"); err == nil {
		t.Fatal("expected error for unknown icon")
	}
}

// Every glyph of the catalogue is a square PNG of 128 pixels, the size a
// profile's tile is drawn at.
func TestCatalogueGlyphsArePNGs(t *testing.T) {
	ids, err := CatalogueIconIDs()
	if err != nil {
		t.Fatalf("CatalogueIconIDs: %v", err)
	}
	for _, id := range ids {
		uri, err := ResolveIcon(id)
		if err != nil {
			t.Fatalf("ResolveIcon(%s): %v", id, err)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, dataURIPrefix))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s is not a PNG: %v", id, err)
		}
		if cfg.Width != 128 || cfg.Height != 128 {
			t.Errorf("%s is %dx%d, not 128x128", id, cfg.Width, cfg.Height)
		}
		if len(uri) > 65536 {
			t.Errorf("%s is %d characters, more than a profile's logo may be", id, len(uri))
		}
	}
}

func TestEncodeCustomLogoRefusesSVG(t *testing.T) {
	if _, err := EncodeCustomLogo([]byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")); err == nil {
		t.Fatal("an SVG was encoded as a tile")
	}
	uri, err := EncodeCustomLogo(pngSignature)
	if err != nil || !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("EncodeCustomLogo(png) = %q, %v", uri, err)
	}
}
