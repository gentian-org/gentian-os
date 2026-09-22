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
// Package tiles is the list of the kernel's own UIs and who may see each.
//
// Tiles are not Components: nothing installs them per tenant, and nothing
// decides at runtime what they are. They are a fixed list shipped with the
// director, filtered by relations on the cluster the way app tiles are
// filtered by can_launch, and addressed under the kernel domain the Cluster
// claim declares.
package tiles

import (
	_ "embed"
	"fmt"

	"sigs.k8s.io/yaml"
)

//go:embed tiles.yaml
var raw []byte

// Tile is one kernel UI.
type Tile struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Description string   `json:"description"`
	SubDomain   string   `json:"subDomain"`
	Path        string   `json:"path,omitempty"`
	Icon        string   `json:"icon"`
	AnyOf       []string `json:"anyOf"`
}

// URL is where the tile leads on a cluster with the given kernel domain.
func (t Tile) URL(kernelDomain string) string {
	path := t.Path
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("https://%s.%s%s", t.SubDomain, kernelDomain, path)
}

var all []Tile

func init() {
	var doc struct {
		Tiles []Tile `json:"tiles"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		panic("tiles.yaml: " + err.Error())
	}
	for _, t := range doc.Tiles {
		if t.Name == "" || t.SubDomain == "" || len(t.AnyOf) == 0 {
			panic("tiles.yaml: every tile needs a name, a subDomain and at least one relation: " + t.Name)
		}
	}
	all = doc.Tiles
}

// All returns every kernel tile, in display order.
func All() []Tile { return append([]Tile(nil), all...) }

// Relations returns every relation any tile names, once: what the
// vocabulary check verifies against the model.
func Relations() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range all {
		for _, r := range t.AnyOf {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out
}
