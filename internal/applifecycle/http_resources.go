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

package applifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gentian-org/gentian-os/internal/resourceplan"
)

const (
	// defaultUsageWindow is what a caller gets when it asks for history without
	// saying how much. Thirty days covers a billing period plus the days either
	// side of it that make a change legible.
	defaultUsageWindow = 30 * 24 * time.Hour
	// maxUsageWindow bounds one request. A tenant's samples are its own and
	// small, but a cluster-wide view multiplies whatever this allows by the
	// tenant count, and 400 days is already more than any invoice needs.
	maxUsageWindow = 400 * 24 * time.Hour
)

func (h *HTTPServer) registerResourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tenants/{tenant}/resources", h.handleResourceState)
	mux.HandleFunc("GET /v1/tenants/{tenant}/resources/plans", h.handleResourcePlans)
	mux.HandleFunc("PUT /v1/tenants/{tenant}/resources", h.handleSetResourcePlan)
	mux.HandleFunc("GET /v1/tenants/{tenant}/resources/usage", h.handleResourceUsage)
	mux.HandleFunc("GET /v1/tenants/{tenant}/resources/report", h.handleResourceReport)
}

func (h *HTTPServer) handleResourceState(w http.ResponseWriter, r *http.Request) {
	state, err := h.Service.ResourceState(r.Context(), r.PathValue("tenant"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *HTTPServer) handleResourcePlans(w http.ResponseWriter, r *http.Request) {
	// No maxTier parameter: the entitlement ceiling is read from the Tenant, so
	// a caller cannot raise it by leaving a field out. selfService remains a
	// caller assertion because only the caller's own front end knows whether a
	// tenant administrator or a platform operator is asking, and it can only
	// ever withhold plans.
	plans, err := h.Service.Plans(
		r.Context(),
		r.PathValue("tenant"),
		boolParam(r, "selfService"),
	)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant": r.PathValue("tenant"),
		"plans":  plans,
	})
}

func (h *HTTPServer) handleSetResourcePlan(w http.ResponseWriter, r *http.Request) {
	actor := r.Header.Get("X-Gentian-Actor")
	if actor == "" {
		actor = "app-lifecycle-api"
	}

	// PUT with a plan name, and no endpoint anywhere that accepts quantities.
	// That is the whole point of the catalogue: a ceiling reachable through
	// this API is always one the platform has priced, so a month of usage
	// resolves to SKUs rather than to numbers someone has to interpret.
	var body struct {
		Plan        string `json:"plan"`
		SelfService bool   `json:"selfService"`
		Force       bool   `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if body.Plan == "" {
		writeErr(w, http.StatusBadRequest, errors.New("plan is required"))
		return
	}

	result, err := h.Service.SetPlan(r.Context(), SetPlanRequest{
		Tenant:      r.PathValue("tenant"),
		Plan:        body.Plan,
		Actor:       actor,
		SelfService: body.SelfService,
		Force:       body.Force,
	})
	if err != nil {
		writeErr(w, resourceErrorStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *HTTPServer) handleResourceUsage(w http.ResponseWriter, r *http.Request) {
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

	samples, err := h.Service.UsageHistory(r.Context(), r.PathValue("tenant"), from, to, step)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":      r.PathValue("tenant"),
		"from":        from,
		"to":          to,
		"stepSeconds": int64(step.Seconds()),
		"samples":     samples,
	})
}

func (h *HTTPServer) handleResourceReport(w http.ResponseWriter, r *http.Request) {
	from, to, err := usageWindow(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	report, err := h.Service.UsageReport(r.Context(), r.PathValue("tenant"), from, to)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// resourceErrorStatus maps a plan failure onto the status a client can act on.
//
// A downgrade that does not fit is 409, not 400: the request is well formed and
// would be accepted at another time or after the tenant frees something. A
// client that retries a 400 verbatim is wrong; one that retries this after
// uninstalling an app is right.
func resourceErrorStatus(err error) int {
	var downgrade *resourceplan.DowngradeError
	switch {
	case errors.As(err, &downgrade):
		return http.StatusConflict
	case errors.Is(err, ErrPlanNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrPlanNotSelectable):
		// 402, matching the App Store's answer for a Pro app the tenant has not
		// bought: the plan is real, the request is valid, and what is missing
		// is an entitlement rather than a permission.
		return http.StatusPaymentRequired
	default:
		return http.StatusBadRequest
	}
}

func usageWindow(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	to := time.Now().UTC()
	if v := q.Get("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid to: %w", err)
		}
		to = parsed.UTC()
	}
	from := to.Add(-defaultUsageWindow)
	if v := q.Get("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid from: %w", err)
		}
		from = parsed.UTC()
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, errors.New("to must be after from")
	}
	if to.Sub(from) > maxUsageWindow {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"window is longer than the %d-day maximum", int(maxUsageWindow.Hours()/24))
	}
	return from, to, nil
}

// usageStep resolves the thinning interval, defaulting to roughly 200 points
// across the window.
//
// Defaulted from the window rather than fixed, because the same endpoint serves
// a day and a year and neither wants the other's resolution. Two hundred is
// about what a chart can draw without overplotting, and it means a caller that
// specifies nothing gets a series it can render.
func usageStep(r *http.Request, window time.Duration) (time.Duration, error) {
	v := r.URL.Query().Get("stepSeconds")
	if v == "" {
		step := window / 200
		if step < time.Minute {
			step = time.Minute
		}
		return step, nil
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0, errors.New("stepSeconds must be a positive integer")
	}
	return time.Duration(secs) * time.Second, nil
}

func boolParam(r *http.Request, name string) bool {
	v := r.URL.Query().Get(name)
	return v == "true" || v == "1"
}
