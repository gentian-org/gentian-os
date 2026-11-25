/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"context"
	"net/http"
	"slices"
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// An app's own Authorization header, approved with its install.
//
// An entry behind sign-in that keeps the app's own Authorization header
// (clientAuthorization: app) is served only once the tenant's perimeter
// approver approves it (exposure_requests.go); until then the header is
// removed and the app's own calls are refused. Whoever installs the app and
// holds can_expose can answer in the install itself, as an egress request is
// answered (install_asks.go): the director lists the entries with what the
// build asks, the install names those approved, and each is recorded as the
// approver's own approval is, by that person.
//
// That kind alone. An entry that puts something on the internet, or takes a
// token from outside, is answered where it is listed with the tenant's
// public addresses, with what it publishes in front of the approver.

// askedExposure is one entry of a build about to be installed that waits for
// the perimeter approver.
type askedExposure struct {
	// Install is the app the entry is of: the app itself, or one it comes
	// with.
	Install string `json:"install"`
	// Exposure is the entry's name, as an approval names it.
	Exposure string `json:"exposure"`
	// Kind is what approving it means (exposure_requests.go):
	// signInAppAuthorization.
	Kind string `json:"kind"`
	// CameWith names the app this one comes with, for an entry that is not
	// the app's own.
	CameWith string `json:"cameWith,omitempty"`

	// answered says the entry waits for nobody: it is approved already and
	// the approval has not expired.
	answered bool
}

// exposedEntry is one entry an install names as approved.
type exposedEntry struct {
	Install  string `json:"install"`
	Exposure string `json:"exposure"`
}

// exposureAsksOf are the entries one profile, installed as install, asks
// the perimeter approver for that may be approved with an install, each
// with whether it waits for nobody.
func exposureAsksOf(install string, definition *gentianov1alpha1.ComponentProfile, cameWith string,
	published []gitops.Exposure, now time.Time,
) []askedExposure {
	var out []askedExposure
	for _, e := range entriesOf(definition) {
		if e.RequestKind() != gentianov1alpha1.ExposureKindSignInAppAuthorization {
			continue
		}
		key := gitops.Exposure{Install: install, ExposureName: e.Name}.Key()
		approved := slices.ContainsFunc(published, func(p gitops.Exposure) bool {
			return p.Key() == key && p.Kind == kindSignInAppAuthorization && stateOf(p, now) != exposureExpired
		})
		out = append(out, askedExposure{
			Install: install, Exposure: e.Name, Kind: kindSignInAppAuthorization, CameWith: cameWith,
			answered: approved,
		})
	}
	return out
}

// notExposureAsked names the first entry an install approves that the build
// and the apps it comes with do not ask for, or "".
func notExposureAsked(expose []exposedEntry, asks []askedExposure) string {
	for _, x := range expose {
		if !slices.ContainsFunc(asks, func(a askedExposure) bool { return a.Install == x.Install && a.Exposure == x.Exposure }) {
			return x.Install + " entry " + x.Exposure
		}
	}
	return ""
}

// exposeSummary is the entries an install approves, for a refusal.
func exposeSummary(expose []exposedEntry) string {
	out := ""
	for i, x := range expose {
		if i > 0 {
			out += ", "
		}
		out += x.Install + " entry " + x.Exposure
	}
	return out
}

// mayExposeWithInstall settles, before anything is fetched or written, that
// the caller may approve the entries an install names: can_expose on the
// tenant, as where the tenant's public addresses are approved. It writes the
// refusal itself and reports false. The decision it returns is what the
// approvals' commits record.
func (s *Server) mayExposeWithInstall(w http.ResponseWriter, r *http.Request, c call, tenant string, expose []exposedEntry) (string, bool) {
	if len(expose) == 0 {
		return "", true
	}
	for _, x := range expose {
		if !gitops.ValidName(x.Install) || !gitops.ValidName(x.Exposure) {
			s.fail(w, r, http.StatusBadRequest, `expose names entries: [{"install": "<app>", "exposure": "<entry>"}]`)
			return "", false
		}
	}
	target := authz.Tenant(tenant)
	ok, err := s.can(r.Context(), c, "can_expose", target)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "authorization unavailable")
		return "", false
	}
	if !ok {
		s.fail(w, r, http.StatusForbidden,
			"approving that an app keeps its own Authorization header is the perimeter approver's; install the app without expose, "+
				"and it waits for their answer. Nothing was installed")
		return "", false
	}
	return "can_expose " + target, true
}

// exposeWithInstall records the entries an install approved, once the install
// is written: one approval each, by the caller, for every entry that still
// waits -- the record the perimeter approver's own approval makes, reviewed
// in a year. One that cannot be written is logged and waits for the
// approver, as it would have without the install naming it.
func (s *Server) exposeWithInstall(ctx context.Context, c call, tenant, decision string, expose []exposedEntry, asks []askedExposure) []exposedEntry {
	exposed := []exposedEntry{}
	meta := c.meta
	meta.Decision = decision
	now := time.Now()
	for _, x := range expose {
		waits := slices.ContainsFunc(asks, func(a askedExposure) bool {
			return a.Install == x.Install && a.Exposure == x.Exposure && !a.answered
		})
		if !waits {
			continue
		}
		_, err := s.cfg.Repo.PublishExposure(ctx, tenant, gitops.Exposure{
			Install:      x.Install,
			ExposureName: x.Exposure,
			Owner:        c.meta.Subject,
			ReviewAt:     now.Add(defaultReview).UTC().Format(time.RFC3339),
			Reason:       "approved with the install of the app",
			Kind:         kindSignInAppAuthorization,
		}, meta)
		if err != nil {
			s.cfg.Log.WarnContext(ctx, "an approval given with an install could not be written; the entry waits",
				"tenant", tenant, "install", x.Install, "exposure", x.Exposure, "error", err.Error())
			continue
		}
		exposed = append(exposed, x)
	}
	return exposed
}
