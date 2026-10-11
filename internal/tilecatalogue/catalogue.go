/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package tilecatalogue is the tile catalogue's format: what the operator
// writes and what the usher reads.
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
	// lives in the control namespace, beside the usher that reads it.
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

	// DisplayName, Description and Icon are what a person sees. DisplayNames
	// are translations of the label keyed by locale; the portal picks the
	// viewer's and falls back to DisplayName, so a catalogue with no
	// translations renders exactly as before.
	DisplayName  string            `json:"displayName"`
	DisplayNames map[string]string `json:"displayNames,omitempty"`
	Description  string            `json:"description,omitempty"`
	// Icon is what the portal draws, and it is one of two things: an SVG as a
	// data URI, which is what every component ships and what a tile from a
	// ComponentProfile always carries, or a name out of the portal's own set,
	// which only the kernel's own consoles use. The portal tells them apart by
	// the data: prefix.
	Icon string `json:"icon"`

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
	// AppStore is whether this cluster offers an App Store, as the operator
	// decided it. Beside the tiles because it is the same kind of statement:
	// something the operator knows about the cluster and the usher hands on.
	// Absent means the operator said nothing, which is read as not offered.
	AppStore *AppStore `json:"appStore,omitempty"`
}

// AppStore is the operator's verdict on whether the cluster offers an App
// Store. It is the same verdict that places the App Store app on tenants, so
// what the usher answers and whether the app is there cannot differ.
type AppStore struct {
	// Offered is whether licence reporting is on and the Cluster claim names
	// a store that is not one of this cluster's own hosts.
	Offered bool `json:"offered"`
	// Reason says why not, as one of the AppStoreReason words.
	Reason string `json:"reason,omitempty"`
}

// Why a cluster offers no App Store.
const (
	// AppStoreReasonNoLicenceReport: the cluster does not report what it
	// runs, and an app installed through a store is what the report lists.
	AppStoreReasonNoLicenceReport = "licence-report-disabled"
	// AppStoreReasonNoStore: the Cluster claim names no store
	// (spec.catalogue.storeUrl), or names one that is not an https address.
	AppStoreReasonNoStore = "no-store-configured"
	// AppStoreReasonOwnHost: the address the Cluster claim names has the host
	// this cluster serves a tenant's App Store app on (store.<the tenant's
	// domain>), so what answers there is the app and not a store.
	AppStoreReasonOwnHost = "store-address-is-own-host"
)

// header explains the projected file to whoever opens the ConfigMap, which is
// the one place a person meets it without reading this package first.
const header = `# The tiles this cluster offers, projected by the operator, and whether it
# offers an App Store (appStore: licence reporting is on and the Cluster claim
# names a store).
#
# Every entry is something the operator routes: a kernel console it composes an
# HTTPRoute for, or an exposure of an installed component whose profile
# declares a tile. Nothing here decides who sees what. The usher asks the
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
	return Catalogue{Tiles: kept, AppStore: c.AppStore}, nil
}

// URL is <scheme>://<host><path>, with the front page when no path is given.
func URL(host, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "https://" + host + path
}
