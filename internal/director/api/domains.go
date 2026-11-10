/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"net/http"

	"github.com/gentian-org/gentian-os/internal/applifecycle"
)

// clusterDomains is GET /v1/clusters/{c}/domains: every domain the cluster
// serves, the DNS records it needs, and whether the internet has them --
// present, pending, missing, or held by somebody else's record, with what is
// served instead. The operator's DNS check finds it out; this passes on its
// last answer as written, for the Operations Console and
// `kubectl gentian domains`.
func (s *Server) clusterDomains(w http.ResponseWriter, r *http.Request, _ call) {
	if s.cfg.Lifecycle == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "the director has no operator to ask")
		return
	}
	status, body, err := s.cfg.Lifecycle.Get(r.Context(), applifecycle.DomainsPath, nil)
	if err != nil || status != http.StatusOK {
		s.fail(w, r, http.StatusBadGateway, "the cluster's DNS status could not be read: "+upstreamFailure(status, body, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
