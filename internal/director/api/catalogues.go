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
// None of this is a licence check. A source named on the Cluster claim is
// offered to every tenant; whether an install is allowed is the person's
// can_install_app and nothing else.

type catalogueOut struct {
	Name string `json:"name"`
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
	// Installable says this entry can be installed from this list: it states
	// its digest, which is what an install from a source is pinned to. False
	// sends the person to the store.
	Installable bool `json:"installable"`
}

// declared reports whether the Cluster claim names a source.
func (s *Server) declared(name string) bool {
	for _, src := range s.cfg.CatalogueSources {
		if src.Name == name {
			return true
		}
	}
	return false
}

// listCatalogues answers what this cluster could install from at all.
func (s *Server) listCatalogues(w http.ResponseWriter, r *http.Request, _ call) {
	tenant := r.PathValue("t")
	out := make([]catalogueOut, 0, len(s.cfg.CatalogueSources))
	for _, src := range s.cfg.CatalogueSources {
		out = append(out, catalogueOut{Name: src.Name})
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "storeUrl": s.cfg.StoreURL, "catalogues": out,
	})
}

// listCatalogueEntries answers what is in one of them.
func (s *Server) listCatalogueEntries(w http.ResponseWriter, r *http.Request, _ call) {
	tenant, name := r.PathValue("t"), r.PathValue("s")
	if !s.declared(name) {
		s.fail(w, r, http.StatusNotFound, "this cluster has no catalogue by that name")
		return
	}
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
			// Without a digest there is nothing to pin an install to, and
			// the store is the route.
			Installable: e.Digest != "",
		})
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "catalogue": name,
		"storeUrl": s.cfg.StoreURL, "entries": entries, "storeOnly": listing.StoreOnly,
	})
}
