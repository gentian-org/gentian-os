/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"errors"
	"net/http"
)

// PlacementsPath is where the operator answers which placeable components
// the platform puts in which tenants.
const PlacementsPath = "/v1/placements"

// registerPlacementRoutes registers the one read of the placement.
//
// The director's placement worker asks it and writes the answer into each
// tenant's manifest. The decision is made here because what it depends on
// is the cluster's: the profiles on it, whether it reports usage statistics,
// whether it offers a store. The director holds no cluster credential to
// read those, and it writes what this answers into git.
func (h *HTTPServer) registerPlacementRoutes(mux router) {
	mux.Read("GET "+PlacementsPath, h.handlePlacements)
}

func (h *HTTPServer) handlePlacements(w http.ResponseWriter, r *http.Request) {
	if h.Placements == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("this operator does not place components"))
		return
	}
	answer, err := h.Placements(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}
