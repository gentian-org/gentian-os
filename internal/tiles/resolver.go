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
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
)

// The catalogue is the platform's glyphs, each a PNG rendered from its
// template in glyphs/ by scripts/tools/render-tiles.py. The templates are
// not embedded: nothing draws or serves an SVG.
//
//go:embed catalogue.json
var catalogueJSON []byte

const (
	defaultIconID = "app"
	dataURIPrefix = "data:image/png;base64,"
)

// pngSignature is the eight bytes every PNG begins with.
var pngSignature = []byte("\x89PNG\r\n\x1a\n")

type catalogue struct {
	Version int                      `json:"version"`
	Tiles   map[string]catalogueTile `json:"tiles"`
}

type catalogueTile struct {
	Label   string `json:"label"`
	DataURI string `json:"dataUri"`
}

var (
	loadOnce sync.Once
	cat      catalogue
	loadErr  error
)

func load() {
	loadOnce.Do(func() {
		loadErr = json.Unmarshal(catalogueJSON, &cat)
	})
}

// CatalogueIconIDs returns sorted tile icon ids from the embedded catalogue.
func CatalogueIconIDs() ([]string, error) {
	load()
	if loadErr != nil {
		return nil, loadErr
	}
	ids := make([]string, 0, len(cat.Tiles))
	for id := range cat.Tiles {
		ids = append(ids, id)
	}
	return ids, nil
}

// ResolveIcon returns the data URI for a catalogue icon id.
func ResolveIcon(iconID string) (string, error) {
	load()
	if loadErr != nil {
		return "", loadErr
	}
	if iconID == "" {
		iconID = defaultIconID
	}
	tile, ok := cat.Tiles[iconID]
	if !ok {
		return "", fmt.Errorf("unknown tile icon %q", iconID)
	}
	if tile.DataURI == "" {
		return "", fmt.Errorf("tile icon %q has empty dataUri", iconID)
	}
	return tile.DataURI, nil
}

// EncodeCustomLogo converts the bytes of a PNG into a Gentian tile data URI.
// Anything that is not a PNG is refused: an SVG is only ever the template a
// tile is rendered from (scripts/tools/render-tiles.py), never the tile.
func EncodeCustomLogo(png []byte) (string, error) {
	if !bytes.HasPrefix(png, pngSignature) {
		return "", fmt.Errorf("a tile is a PNG, and this is not one")
	}
	return dataURIPrefix + base64.StdEncoding.EncodeToString(png), nil
}
