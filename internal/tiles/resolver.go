package tiles

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

//go:embed catalogue.json
var catalogueJSON []byte

const (
	defaultIconID = "app"
	dataURIPrefix = "data:image/svg+xml;base64,"
)

type catalogue struct {
	Version int `json:"version"`
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

// ResolveLogo picks the portal tile logo using profile and optional per-tile overrides.
// Priority: tileSpec > profileSpec > legacy fields > default catalogue icon.
func ResolveLogo(
	profileTile *gentianov1alpha1.TileSpec,
	profileLegacyLogo string,
	portalTile *gentianov1alpha1.TileSpec,
	portalLegacyLogo string,
) (string, error) {
	if uri, ok := resolveTileSpec(portalTile); ok {
		return uri, nil
	}
	if portalLegacyLogo != "" {
		return normalizeDataURI(portalLegacyLogo), nil
	}
	if uri, ok := resolveTileSpec(profileTile); ok {
		return uri, nil
	}
	if profileLegacyLogo != "" {
		return normalizeDataURI(profileLegacyLogo), nil
	}
	return ResolveIcon(defaultIconID)
}

func resolveTileSpec(spec *gentianov1alpha1.TileSpec) (string, bool) {
	if spec == nil {
		return "", false
	}
	if spec.Logo != "" {
		return normalizeDataURI(spec.Logo), true
	}
	if spec.Icon != "" {
		uri, err := ResolveIcon(spec.Icon)
		if err != nil {
			return "", false
		}
		return uri, true
	}
	return "", false
}

func normalizeDataURI(value string) string {
	if strings.HasPrefix(value, dataURIPrefix) {
		return value
	}
	return dataURIPrefix + strings.TrimPrefix(value, dataURIPrefix)
}

// LogoBase64 returns the base64 payload without the data URI prefix for portal tile jobs.
func LogoBase64(dataURI string) string {
	return strings.TrimPrefix(dataURI, dataURIPrefix)
}

// EncodeCustomLogo converts raw SVG bytes into a Gentian tile data URI.
func EncodeCustomLogo(svg []byte) string {
	return dataURIPrefix + base64.StdEncoding.EncodeToString(svg)
}
