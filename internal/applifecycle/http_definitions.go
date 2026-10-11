/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"net/http"

	"github.com/gentian-org/gentian-os/internal/schemacheck"
)

// DefinitionsPath is where the operator answers what the cluster serves of
// the resource definitions this software was built with.
const DefinitionsPath = "/v1/definitions"

// registerDefinitionRoutes registers the one read of the definitions check.
//
// A read, registered as the other reads are, for two callers. The director
// asks before a commit: it writes manifests the cluster then prunes against
// these definitions, it holds no cluster credential to look for itself, and
// it refuses a write the answer does not cover. And an operations screen
// reads the same answer through the usher's identity, which the reads of this
// listener already admit -- so showing it needs no permission anyone lacks.
//
// The answer is the operator's memory of its last check, not a read of the
// cluster: this route reaches nothing and cannot be made to.
func (h *HTTPServer) registerDefinitionRoutes(mux router) {
	mux.Read("GET "+DefinitionsPath, h.handleDefinitions)
}

func (h *HTTPServer) handleDefinitions(w http.ResponseWriter, _ *http.Request) {
	if h.Definitions == nil {
		// Answered, and as "not checked": a caller that must not assume the
		// definitions are current reads this exactly as it should.
		writeJSON(w, http.StatusOK, schemacheck.Report{Definitions: schemacheck.Embedded().Digest, Kinds: []schemacheck.KindReport{}})
		return
	}
	report := h.Definitions.Report()
	if report.Kinds == nil {
		report.Kinds = []schemacheck.KindReport{}
	}
	writeJSON(w, http.StatusOK, report)
}
