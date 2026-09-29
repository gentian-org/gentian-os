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
// So it lists coordinates, versions and editions, and nothing that would
// pretend to be a shop. Two consequences follow, and both are deliberate:
//
//   - Only ce and pe are listed. me and ee exist because somebody maintains
//     or licenses them; an entry whose whole value is a relationship with a
//     supplier is not something a cluster can describe usefully, so it is
//     counted and named to the store instead.
//   - An ENTITLED source's entries carry no digest here. The digest that
//     governs an install from such a source is the one the store stated over
//     its own TLS; a source's own number, checked against the same source's
//     own bytes, is not a check (AD-3). An open source's digest is served,
//     because there the trust is the Cluster claim naming the source and
//     there is no store in the picture to say otherwise.

type catalogueOut struct {
	Name   string `json:"name"`
	Access string `json:"access"`
	// Open says this source is open to THIS tenant -- it may install from it
	// with no statement from the store. Straight from the claim, not from a
	// graph check: the claim is what decides it, and an entry's tuple is only
	// written when somebody first installs it.
	Open bool `json:"open"`
}

type entryOut struct {
	Coordinate string `json:"coordinate"`
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	Edition    string `json:"edition"`
	TrustTier  string `json:"trustTier,omitempty"`
	Digest     string `json:"digest,omitempty"`
	// Installable says the tenant may install this entry from here, now,
	// without going anywhere. False does not mean refused -- it means the
	// store decides, and the store is where to go.
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
		access := src.Access
		if access == "" {
			access = "entitled"
		}
		out = append(out, catalogueOut{Name: src.Name, Access: access, Open: src.Open(tenant)})
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
	listing, err := s.cfg.Catalogue.Index(r.Context(), name, !open)
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
			// Installable from here exactly when the claim opened this source
			// to this tenant. Otherwise the store decides, and an entry it
			// has already granted is installed from the store's own screen
			// with the store's own digest -- not from this list.
			Installable: open && e.Digest != "",
		})
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant": tenant, "catalogue": name, "open": open,
		"storeUrl": s.cfg.StoreURL, "entries": entries, "storeOnly": listing.StoreOnly,
	})
}
