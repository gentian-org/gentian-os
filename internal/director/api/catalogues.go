/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"errors"
	"net/http"

	"github.com/gentian-org/gentian-os/internal/director/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// What a cluster can say about its own catalogues, and what it deliberately
// cannot (AD-14).
//
// The App Store is the route. It knows what an app is for, what it costs, who
// maintains it and what the screenshots look like, and it is kept current by
// people whose job that is. This is what remains when the store is not the
// answer: the store is unreachable, or the cluster is air-gapped, or -- the
// case that has no store answer at all -- the entry is the operator's own
// profile in the operator's own repository, which nobody sells and nobody
// else lists.
//
// So it lists coordinates, versions, editions and digests, and nothing that
// would pretend to be a shop. Only ce and pe are listed: me and ee exist
// because somebody maintains or licenses them, and an entry whose whole value
// is a relationship with a supplier is not something a cluster can describe
// usefully, so it is counted and named to the store instead.
//
// None of this is a licence check. A source being open to a tenant says the
// cluster offers that source's entries to it here; it does not decide whether
// an install is allowed, which is the person's can_install_app and nothing
// else.

type catalogueOut struct {
	Name string `json:"name"`
	// Open says the Cluster claim opens this source to THIS tenant. Straight
	// from the claim, which is what decides it.
	Open bool `json:"open"`
}

type entryOut struct {
	Coordinate string `json:"coordinate"`
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	Edition    string `json:"edition"`
	TrustTier  string `json:"trustTier,omitempty"`
	// Digest is the build the source lists: what an install of this entry
	// from here sends back.
	Digest string `json:"digest,omitempty"`
	// Installable says the cluster offers this entry to the tenant from this
	// list: the source is open to it and the entry states its digest. False
	// sends the person to the store.
	Installable bool `json:"installable"`
}

// source finds a declared source by name.
func (s *Server) source(name string) (gitops.CatalogueSource, bool) {
	for _, src := range s.cfg.CatalogueSources {
		if src.Name == name {
			return src, true
		}
	}
	return gitops.CatalogueSource{}, false
}

// listCatalogues answers what this cluster could install from at all.
func (s *Server) listCatalogues(w http.ResponseWriter, r *http.Request, _ call) {
	tenant := r.PathValue("t")
	out := make([]catalogueOut, 0, len(s.cfg.CatalogueSources))
	for _, src := range s.cfg.CatalogueSources {
		out = append(out, catalogueOut{Name: src.Name, Open: src.Open(tenant)})
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "storeUrl": s.cfg.StoreURL, "catalogues": out,
	})
}

// listCatalogueEntries answers what is in one of them.
func (s *Server) listCatalogueEntries(w http.ResponseWriter, r *http.Request, _ call) {
	tenant, name := r.PathValue("t"), r.PathValue("s")
	src, ok := s.source(name)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "this cluster has no catalogue by that name")
		return
	}
	open := src.Open(tenant)
	listing, err := s.cfg.Catalogue.Index(r.Context(), name)
	switch {
	case errors.Is(err, catalogue.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "this cluster has no catalogue by that name")
		return
	case err != nil:
		// The source is somebody else's web server and it is allowed to be
		// down. Said as a gateway error rather than a server error, because
		// nothing here is broken.
		s.cfg.Log.WarnContext(r.Context(), "a catalogue source's index could not be read",
			"request_id", reqID(r.Context()), "catalogue", name, "error", err)
		s.fail(w, r, http.StatusBadGateway, "the catalogue source could not be read")
		return
	}

	entries := make([]entryOut, 0, len(listing.Entries))
	for _, e := range listing.Entries {
		entries = append(entries, entryOut{
			Coordinate: name + "/" + e.Name,
			Name:       e.Name,
			Version:    e.Version,
			Edition:    string(e.Edition),
			TrustTier:  e.TrustTier,
			Digest:     e.Digest,
			// Offered from here exactly when the claim opened this source to
			// this tenant; otherwise the store is the route.
			Installable: open && e.Digest != "",
		})
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "catalogue": name, "open": open,
		"storeUrl": s.cfg.StoreURL, "entries": entries, "storeOnly": listing.StoreOnly,
	})
}
