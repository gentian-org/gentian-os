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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

//go:embed default.tokens.json
var defaultTokens []byte

// The platform's own name, shown when the cluster sets none.
const (
	DefaultName      = "Gentian"
	DefaultShortName = "Gentian"
)

// The files a brand is published as, beside the concierge at
// id.<kernel>/branding/, which every page loads.
const (
	CSSFile      = "brand.css"
	IdentityFile = "brand.json"
	ManifestFile = "brand.webmanifest"
)

// Identity is brand.json: what a page needs that is not a style.
type Identity struct {
	Name                 string                       `json:"name"`
	ShortName            string                       `json:"shortName"`
	Description          string                       `json:"description,omitempty"`
	Icons                []gentianov1alpha1.BrandIcon `json:"icons,omitempty"`
	HideVendorPromotions bool                         `json:"hideVendorPromotions,omitempty"`
}

// Files is a published brand: text files, and the icons a data URL carried,
// decoded into files of their own so that brand.json and the manifest name
// them once by URL instead of each carrying the image.
type Files struct {
	Text   map[string]string
	Binary map[string][]byte
}

// iconExtensions maps the data URL media types BrandIcon admits.
var iconExtensions = map[string]string{
	"image/png": "png", "image/svg+xml": "svg", "image/webp": "webp",
	"image/x-icon": "ico", "image/vnd.microsoft.icon": "ico",
}

// Render publishes a brand: nil is the platform's own. The custom tokens
// override the default ones path by path, so a brand that sets only its
// primary colour keeps the rest of the default look.
//
// An icon's src in brand.json and in the manifest is relative to the file it
// is in, as the manifest specification resolves it; a page reading brand.json
// resolves it the same way.
func Render(spec *gentianov1alpha1.BrandingSpec) (Files, error) {
	files := Files{Text: map[string]string{}, Binary: map[string][]byte{}}
	tokens, err := Parse(defaultTokens)
	if err != nil {
		return files, fmt.Errorf("default tokens: %w", err)
	}
	id := Identity{Name: DefaultName, ShortName: DefaultShortName}
	if spec != nil {
		if spec.Tokens != nil && len(spec.Tokens.Raw) > 0 {
			custom, err := Parse(spec.Tokens.Raw)
			if err != nil {
				return files, err
			}
			tokens = merge(tokens, custom)
		}
		if n := strings.TrimSpace(spec.Identity.Name); n != "" {
			id.Name = n
			id.ShortName = n
		}
		if s := strings.TrimSpace(spec.Identity.ShortName); s != "" {
			id.ShortName = s
		}
		id.Description = strings.TrimSpace(spec.Identity.Description)
		for i, ic := range spec.Identity.Icons {
			if strings.HasPrefix(ic.Src, "data:") {
				name, body, err := decodeIcon(i, ic.Src)
				if err != nil {
					return files, err
				}
				files.Binary[name] = body
				ic.Src = name
			}
			id.Icons = append(id.Icons, ic)
		}
		id.HideVendorPromotions = spec.HideVendorPromotions
	}
	identity, err := json.Marshal(id)
	if err != nil {
		return files, err
	}
	manifest, err := json.MarshalIndent(webManifest(id, tokens), "", "  ")
	if err != nil {
		return files, err
	}
	files.Text[CSSFile] = CSS(tokens)
	files.Text[IdentityFile] = string(identity)
	files.Text[ManifestFile] = string(manifest)
	return files, nil
}

// decodeIcon turns a data URL into the file it names.
func decodeIcon(i int, src string) (string, []byte, error) {
	header, data, ok := strings.Cut(strings.TrimPrefix(src, "data:"), ";base64,")
	ext, known := iconExtensions[header]
	if !ok || !known {
		return "", nil, fmt.Errorf("icon %d: not a base64 data URL of an image type a page shows", i)
	}
	body, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", nil, fmt.Errorf("icon %d: %w", i, err)
	}
	return fmt.Sprintf("icon-%d.%s", i, ext), body, nil
}

func merge(base, over []Token) []Token {
	byPath := map[string]int{}
	out := append([]Token(nil), base...)
	for i, t := range out {
		byPath[strings.Join(t.Path, ".")] = i
	}
	for _, t := range over {
		if i, ok := byPath[strings.Join(t.Path, ".")]; ok {
			out[i] = t
			continue
		}
		out = append(out, t)
	}
	return out
}

// webManifest is the Web App Manifest an installed page uses: the brand's
// name and icons, its primary colour as the theme and its page colour as
// the background.
func webManifest(id Identity, tokens []Token) map[string]any {
	m := map[string]any{
		"name":       id.Name,
		"short_name": id.ShortName,
		"display":    "standalone",
	}
	if id.Description != "" {
		m["description"] = id.Description
	}
	if len(id.Icons) > 0 {
		icons := make([]map[string]string, 0, len(id.Icons))
		for _, ic := range id.Icons {
			icon := map[string]string{"src": ic.Src}
			if ic.Sizes != "" {
				icon["sizes"] = ic.Sizes
			}
			if ic.Type != "" {
				icon["type"] = ic.Type
			}
			if ic.Purpose != "" {
				icon["purpose"] = ic.Purpose
			}
			icons = append(icons, icon)
		}
		m["icons"] = icons
	}
	for _, t := range tokens {
		if strings.HasPrefix(t.CSS, "var(") {
			continue
		}
		switch strings.Join(t.Path, ".") {
		case "color.brand.500":
			m["theme_color"] = t.CSS
		case "color.paper.0":
			m["background_color"] = t.CSS
		}
	}
	return m
}
