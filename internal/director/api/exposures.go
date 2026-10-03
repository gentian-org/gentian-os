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
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Publishing a surface to the internet (AD-6).
//
// A profile declares that it COULD publish something; a perimeter approver
// decides that it DOES, for a host, until a date. These are that decision,
// and the read is the registry of everything this tenant has on the internet.

// maxExposure is how long one enablement may run before somebody has to look
// at it again.
//
// A year, and not "no expiry", because the failure this bounds is the one
// nobody notices: a surface published for a reason that stopped being true,
// still answering because removing it was never anybody's task. An expiry
// makes forgetting the safe outcome instead of the dangerous one.
const maxExposure = 365 * 24 * time.Hour

// defaultExposure is what a caller gets for saying nothing. Short enough that
// an experiment expires on its own, long enough to be useful.
const defaultExposure = 90 * 24 * time.Hour

type publishExposureRequest struct {
	// ExpiresAt bounds it, RFC 3339. Empty means the default.
	ExpiresAt string `json:"expiresAt,omitempty"`
	// ReviewAt is when to ask again, before expiry.
	ReviewAt string `json:"reviewAt,omitempty"`
	// Reason is why this is public, in the approver's words.
	Reason string `json:"reason,omitempty"`
}

// tenantExposures answers what this tenant publishes: the registry.
func (s *Server) tenantExposures(w http.ResponseWriter, r *http.Request, _ call) {
	published, err := s.cfg.Repo.TenantExposures(r.Context(), r.PathValue("t"))
	if err != nil {
		s.repoError(w, r, err)
		return
	}
	if published == nil {
		published = []gitops.Exposure{}
	}
	// Live and expired are different answers, and a screen that showed them
	// the same would report a tenant as publishing something it no longer is.
	now := time.Now()
	live := make([]gitops.Exposure, 0, len(published))
	expired := make([]gitops.Exposure, 0)
	for _, e := range published {
		at, err := time.Parse(time.RFC3339, e.ExpiresAt)
		if err == nil && !now.Before(at) {
			expired = append(expired, e)
			continue
		}
		live = append(live, e)
	}
	s.json(w, http.StatusOK, map[string]any{
		"tenant":  r.PathValue("t"),
		"live":    live,
		"expired": expired,
	})
}

// publishExposure records that a perimeter approver put one surface on the
// internet.
func (s *Server) publishExposure(w http.ResponseWriter, r *http.Request, c call) {
	install, exposure := r.PathValue("inst"), r.PathValue("name")
	if install == "" || exposure == "" {
		s.fail(w, r, http.StatusBadRequest, "the path must name an install and an exposure")
		return
	}
	var body publishExposureRequest
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest,
			`body must be {"expiresAt": "<RFC 3339>", "reason": "...", "host": "..."}`)
		return
	}

	expires := time.Now().Add(defaultExposure)
	if body.ExpiresAt != "" {
		at, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "expiresAt must be an RFC 3339 timestamp")
			return
		}
		if !at.After(time.Now()) {
			s.fail(w, r, http.StatusBadRequest, "expiresAt is in the past; that publishes nothing")
			return
		}
		if at.After(time.Now().Add(maxExposure)) {
			// Refused rather than clamped: somebody asking for ten years
			// should be told no, not quietly given one.
			s.fail(w, r, http.StatusBadRequest,
				"a surface may be published for at most a year at a time; renew it instead")
			return
		}
		expires = at
	}

	e := gitops.Exposure{
		Install:      install,
		ExposureName: exposure,
		// From the token, never the body: an enablement that could name its
		// own owner records nobody.
		Owner:     c.meta.Subject,
		ExpiresAt: expires.UTC().Format(time.RFC3339),
		ReviewAt:  strings.TrimSpace(body.ReviewAt),
		Reason:    strings.TrimSpace(body.Reason),
	}
	res, err := s.cfg.Repo.PublishExposure(r.Context(), r.PathValue("t"), e, c.meta)
	if err != nil && errors.Is(err, gitops.ErrInvalidName) {
		// A name that is not a name is the caller's mistake, and the message
		// names the value rather than arriving as a rejected commit.
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// withdrawExposure takes one down.
func (s *Server) withdrawExposure(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.WithdrawExposure(
		r.Context(), r.PathValue("t"), r.PathValue("inst"), r.PathValue("name"), c.meta)
	s.written(w, r, res, err)
}
