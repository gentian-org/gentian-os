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

// Package tilecatalogue is the tile catalogue's format: what the operator
// writes and what the director reads.
//
// It is its own package, and not part of internal/tiles, because that one is
// the portal's icon set. The two share a word and nothing else, and the
// director should not carry an embedded SVG catalogue to read a list of
// links.
//
// The catalogue used to be a list compiled into the director, which meant the
// director held a second, hand-maintained copy of facts the operator already
// owned: which consoles exist, what hostname each answers on, and which
// relation opens it. Two copies of the same thing drift, and an installed app
// could not appear on the portal without a director release, which is the
// wrong answer for a platform whose point is installing apps.
//
// So the catalogue is projected by the operator from what it actually routes,
// and the director does the one thing that is its job: it asks the graph, per
// caller, which of these the caller may open. This package is the format they
// agree on, in one place so that the writer and the reader cannot disagree
// about it.
package tilecatalogue

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	// ConfigMapName is the one ConfigMap the catalogue is projected into. It
	// lives in the control namespace, beside both the operator that writes it
	// and the director that reads it.
	ConfigMapName = "gentian-tiles"

	// Key is the entry within it. The content is YAML rather than one key per
	// tile because a tile is read and replaced whole, and because a person
	// running kubectl describe should see the catalogue as a list.
	Key = "tiles.yaml"
)

// Tile is one link the portal may show.
//
// The URL is complete rather than a host and a path to be assembled, because
// the operator is the side that knows the hostname: it wrote the route that
// makes it answer. Leaving the director to compose it would put the rule for
// building a hostname in two places again, which is what this projection
// exists to stop.
type Tile struct {
	// Name identifies the tile, for the portal and for logs. Unique within
	// one catalogue.
	Name string `json:"name"`

	// DisplayName, Description and Icon are what a person sees.
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon"`

	// URL is where the tile leads, complete and absolute.
	URL string `json:"url"`

	// Object is what the caller's rights are asked about, in OpenFGA's
	// <type>:<id> form: the cluster for a kernel console, the app for a
	// component's own entry.
	Object string `json:"object"`

	// AnyOf are the relations on Object that put this tile on a person's
	// page. Any one of them is enough, which is how a console reached by
	// several roles stays one tile rather than three.
	AnyOf []string `json:"anyOf"`
}

// Catalogue is the file's whole content.
type Catalogue struct {
	Tiles []Tile `json:"tiles"`
}

// header explains the projected file to whoever opens the ConfigMap, which is
// the one place a person meets it without reading this package first.
const header = `# The tiles this cluster offers, projected by the operator.
#
# Every entry is something the operator routes: a kernel console it composes an
# HTTPRoute for, or an exposure of an installed component whose profile
# declares a tile. Nothing here decides who sees what. The director asks the
# authorization graph, per caller, whether that person holds one of the
# relations in anyOf on the object, and answers with the tiles that survive.
#
# Do not edit. The operator replaces this content whenever what it routes
# changes, and an edit made here is lost at the next reconcile.
`

// Marshal renders a catalogue for the ConfigMap.
func Marshal(c Catalogue) (string, error) {
	if c.Tiles == nil {
		c.Tiles = []Tile{}
	}
	body, err := yaml.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("render the tile catalogue: %w", err)
	}
	return header + string(body), nil
}

// Parse reads what Marshal wrote.
//
// An entry missing a name, a URL, an object or a relation is dropped rather
// than failing the read: one malformed tile should cost that tile, not the
// whole page. The reader logs nothing about it because the writer is the
// operator and the operator is where such a thing is diagnosed.
func Parse(data []byte) (Catalogue, error) {
	var c Catalogue
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Catalogue{}, fmt.Errorf("read the tile catalogue: %w", err)
	}
	kept := make([]Tile, 0, len(c.Tiles))
	for _, t := range c.Tiles {
		if t.Name == "" || t.URL == "" || t.Object == "" || len(t.AnyOf) == 0 {
			continue
		}
		kept = append(kept, t)
	}
	return Catalogue{Tiles: kept}, nil
}

// URL is <scheme>://<host><path>, with the front page when no path is given.
func URL(host, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "https://" + host + path
}
