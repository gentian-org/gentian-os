/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// registerResidueRoutes serves what the catalogue left behind (residue.go):
// the list, which the usher answers whoever may audit the cluster with, and
// the removal of one object on it, which is the director's command.
//
// Neither is under a tenant. The objects are the cluster's: Compositions and
// pack catalogs have no namespace, and the rest are in the namespace the
// catalogue is applied in, where a tenant decides nothing.
func (h *HTTPServer) registerResidueRoutes(mux router) {
	mux.Read("GET /v1/catalogue/residue", h.handleCatalogueResidue)
	mux.HandleFunc("POST /v1/actions/remove-catalogue-residue", h.handleRemoveCatalogueResidue)
}

func (h *HTTPServer) handleCatalogueResidue(w http.ResponseWriter, r *http.Request) {
	res, err := h.Service.CatalogueResidue(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *HTTPServer) handleRemoveCatalogueResidue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(`the body names one object: {"kind": …, "name": …, "namespace": …}: %w`, err))
		return
	}
	res, err := h.Service.RemoveResidue(r.Context(), body.Kind, body.Name, body.Namespace, actorOf(r))
	// A refusal says which of three it is beside saying why, because the
	// director treats one of them as "not yet": an object Argo CD still
	// finds declared goes when Argo CD has caught up.
	refused := func(reason string) {
		writeJSON(w, http.StatusConflict, map[string]string{"detail": strings.TrimSpace(err.Error()), "reason": reason})
	}
	switch {
	case errors.Is(err, ErrNotARemovableKind):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, ErrNotResidue):
		refused(RefusedNotResidue)
	case errors.Is(err, ErrStillDeclared):
		refused(RefusedStillDeclared)
	case errors.Is(err, ErrResidueChanged):
		refused(RefusedChanged)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// Why a removal was refused, for a program. Nothing was deleted in any of
// the three.
const (
	RefusedNotResidue    = "not-residue"
	RefusedStillDeclared = "still-declared"
	RefusedChanged       = "changed"
)
