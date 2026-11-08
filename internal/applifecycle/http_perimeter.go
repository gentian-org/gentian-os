/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"net/http"
	"time"

	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/perimetertraffic"
	"github.com/gentian-org/gentian-os/internal/usage"
)

// What a tenant's public entries carried. The usher answers it under
// can_expose: it is the perimeter's, read by those who answer for it.
func (h *HTTPServer) registerPerimeterRoutes(mux router) {
	mux.Read("GET /v1/tenants/{tenant}/perimeter/traffic", h.handlePerimeterTraffic)
}

func (h *HTTPServer) handlePerimeterTraffic(w http.ResponseWriter, r *http.Request) {
	from, to, err := usageWindow(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	step, err := usageStep(r, to.Sub(from))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	totals, series, err := h.Service.PerimeterTraffic(r.Context(), r.PathValue("tenant"), from, to, step)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":      r.PathValue("tenant"),
		"from":        from,
		"to":          to,
		"stepSeconds": int64(step.Seconds()),
		"totals":      totals,
		"series":      series,
		// Said in the answer, so a screen does not claim more than it has.
		"covers": "requests the edge handed to each published path, by response class. " +
			"Requests the edge refused before that -- over the rate limit, malformed -- are not counted here.",
	})
}

// PerimeterTraffic reads a tenant's traffic history: each path's totals over
// the window, busiest first, and the tenant's traffic per step.
func (s *Service) PerimeterTraffic(
	ctx context.Context, tenantName string, from, to time.Time, step time.Duration,
) ([]perimetertraffic.Row, []perimetertraffic.Point, error) {
	base, err := usage.StoreForTenant(ctx, s.client, layout.Tenant(tenantName), tenantName)
	if err != nil {
		return nil, nil, err
	}
	store := perimetertraffic.NewStore(base)
	totals, err := store.Totals(ctx, from, to)
	if err != nil {
		return nil, nil, err
	}
	series, err := store.Series(ctx, from, to, step)
	if err != nil {
		return nil, nil, err
	}
	return totals, series, nil
}
