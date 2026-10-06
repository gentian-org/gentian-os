/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"strings"
)

// Where a cluster's profiles may come from (AD-14).
//
// On the Cluster CLAIM, not in the director's environment. The difference is
// not cosmetic: a source is where software enters this cluster, and opening
// one to a tenant is the platform administrator's act. On the claim it is a
// commit somebody reviewed, with an author and a date, next to everything
// else the cluster is. In an environment variable it is a deployment setting
// that changed when somebody rolled the Deployment, and the only record is
// whatever the pod spec happens to say now.
//
// It also makes the answer to "where could this cluster install software
// from" readable without cluster access, which is the question an audit asks
// and the one an environment variable answers worst.

// CatalogueSource is one repository of profile bundles.
type CatalogueSource struct {
	// Name is the catalogue's slug: the first half of a coordinate, so
	// "main/nextcloud-base-ce" is served by the source named main.
	Name string `json:"name"`
	// URL is where bundles are fetched from.
	URL string `json:"url"`
	// Tenants are the tenants this source is open to: the cluster lists the
	// source's entries to them as installable from here. Empty means none --
	// a source is open to somebody, never to all, and nothing is open by
	// default.
	//
	// It is what the cluster offers, not a licence. An install is asked of
	// the person (can_install_app), and whether the app then arrives is
	// decided where its artefacts are pulled.
	Tenants []string `json:"tenants,omitempty"`
}

// Open reports whether the Cluster claim opens this source to a tenant.
func (s CatalogueSource) Open(tenant string) bool {
	for _, t := range s.Tenants {
		if t == tenant {
			return true
		}
	}
	return false
}

// CatalogueSettings is the claim's whole spec.catalogue.
type CatalogueSettings struct {
	// StoreURL is where people are sent to get the entries this cluster
	// does not list itself -- everything maintained or licensed by somebody.
	//
	// Here rather than compiled in, because which store a cluster belongs to
	// is a fact about that cluster, and because a cluster with no store at
	// all is a supported thing: it says nothing and its own catalogues are
	// all there is.
	StoreURL string `json:"storeUrl,omitempty"`
	// Sources are the catalogues this cluster may fetch from.
	Sources []CatalogueSource `json:"sources,omitempty"`
}

// Catalogue reads spec.catalogue from the Cluster claim.
func (g *GitOps) Catalogue(ctx context.Context) (CatalogueSettings, error) {
	var claim struct {
		Spec struct {
			Catalogue CatalogueSettings `json:"catalogue"`
		} `json:"spec"`
	}
	if err := g.readClusterClaim(ctx, &claim); err != nil {
		return CatalogueSettings{}, err
	}
	out := CatalogueSettings{StoreURL: strings.TrimSpace(claim.Spec.Catalogue.StoreURL)}
	if !strings.HasPrefix(out.StoreURL, "https://") {
		out.StoreURL = ""
	}
	for _, src := range claim.Spec.Catalogue.Sources {
		src.Name = strings.TrimSpace(src.Name)
		src.URL = strings.TrimSpace(src.URL)
		if src.Name == "" || !strings.HasPrefix(src.URL, "https://") {
			// http:// is skipped here as it is everywhere else: the digest
			// makes the bytes safe, but a cluster fetching its catalogue in
			// clear announces what it runs.
			continue
		}
		out.Sources = append(out.Sources, src)
	}
	return out, nil
}

// CatalogueSources reads the sources this cluster declares.
//
// A source with no name or no https URL is skipped rather than refused: the
// claim is read on every start, and one malformed entry must not stop a
// director from serving. What it costs is that installs from that catalogue
// are refused for want of a source, which says the same thing where somebody
// will see it.
func (g *GitOps) CatalogueSources(ctx context.Context) ([]CatalogueSource, error) {
	c, err := g.Catalogue(ctx)
	if err != nil {
		return nil, err
	}
	return c.Sources, nil
}
