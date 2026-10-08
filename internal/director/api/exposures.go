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
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/tenancy"
)

// Publishing a surface to the internet (AD-6).
//
// A profile declares that it COULD publish something; a perimeter approver
// decides that it DOES, and when it is looked at again. These are that decision,
// and the read is the registry of everything this tenant has on the internet.

// maxReview is the longest anything public goes without somebody looking at
// it again.
//
// A year. What this bounds is the surface nobody remembers: published for a
// reason that stopped being true, still answering because removing it was
// never anybody's task. The answer to that is knowing what is public and
// revisiting it -- not an expiry, which makes the forgotten surface go away
// and takes a tenant's website with it the day nobody renewed.
const maxReview = 365 * 24 * time.Hour

// defaultReview is the review date a caller gets for naming none: a year,
// which is also the longest. Once a year is the regular look this asks for.
const defaultReview = maxReview

type publishExposureRequest struct {
	// ReviewAt is when it is looked at again, RFC 3339. Empty means a year
	// from now, and it is never later than that.
	ReviewAt string `json:"reviewAt,omitempty"`
	// ExpiresAt ends it, RFC 3339, for a surface published for a while.
	// Empty means it stays until somebody withdraws it.
	ExpiresAt string `json:"expiresAt,omitempty"`
	// Reason is why this is public, in the approver's words.
	Reason string `json:"reason,omitempty"`
	// Apex asks for the cluster's main address, the bare domain. Only the
	// user tenant of a single-tenancy cluster may, and for one surface at a
	// time. The profile's entry has to be one declared for it (apex: true);
	// the operator checks that and says so on the Component.
	Apex bool `json:"apex,omitempty"`
	// AcknowledgeMainAddressRule is the approver saying they were told what
	// a script on the main address can do to people's sign-in, and that the
	// site is one whose scripts their organisation controls. Required with
	// apex, on a first publication and on every review; refused without
	// (mainAddressWarning). It means nothing on any other surface.
	AcknowledgeMainAddressRule bool `json:"acknowledgeMainAddressRule,omitempty"`
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
	// reviewDue is the live ones whose review date has passed: still
	// published, and somebody owes them a look.
	live, expired, due := sortExposures(published, time.Now())
	s.json(w, http.StatusOK, map[string]any{
		"tenant":    r.PathValue("t"),
		"live":      live,
		"expired":   expired,
		"reviewDue": due,
	})
}

// sortExposures splits a tenant's registry by what is true of each entry now.
func sortExposures(published []gitops.Exposure, now time.Time) (live, expired, reviewDue []gitops.Exposure) {
	live, expired, reviewDue = []gitops.Exposure{}, []gitops.Exposure{}, []gitops.Exposure{}
	for _, e := range published {
		if e.ExpiresAt != "" {
			if at, err := time.Parse(time.RFC3339, e.ExpiresAt); err == nil && !now.Before(at) {
				expired = append(expired, e)
				continue
			}
		}
		live = append(live, e)
		if at, err := time.Parse(time.RFC3339, e.ReviewAt); err != nil || !now.Before(at) {
			reviewDue = append(reviewDue, e)
		}
	}
	return live, expired, reviewDue
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
			`body must be {"reviewAt": "<RFC 3339>", "expiresAt": "<RFC 3339>", "reason": "..."}; all optional`)
		return
	}

	if body.Apex {
		// The main address is the most visible page of the cluster and
		// nobody is signed in on it. Whose it may be is the mode's.
		settings, err := s.cfg.Repo.ClusterSettingValues(r.Context())
		if err != nil && !errors.Is(err, gitops.ErrNoClusterClaim) {
			s.repoError(w, r, err)
			return
		}
		// No claim is no word that the cluster is single, and so it is not.
		if !tenancy.SoleUserTenant(settings["tenancyMode"], r.PathValue("t")) {
			s.fail(w, r, http.StatusConflict, mainAddressRefused)
			return
		}
		// Asked after the mode, so nobody is told to acknowledge a rule for
		// something this cluster would refuse anyway.
		if !body.AcknowledgeMainAddressRule {
			s.fail(w, r, http.StatusBadRequest, mainAddressWarning)
			return
		}
	}

	now := time.Now()
	review := now.Add(defaultReview)
	if v := strings.TrimSpace(body.ReviewAt); v != "" {
		at, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "reviewAt must be an RFC 3339 timestamp")
			return
		}
		if !at.After(now) {
			s.fail(w, r, http.StatusBadRequest, "reviewAt is in the past; a review date is when to look again")
			return
		}
		if at.After(now.Add(maxReview)) {
			// Refused rather than clamped: somebody asking for ten years
			// should be told no, not quietly given one.
			s.fail(w, r, http.StatusBadRequest, "a public surface is reviewed at least once a year")
			return
		}
		review = at
	}
	expires := ""
	if v := strings.TrimSpace(body.ExpiresAt); v != "" {
		at, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "expiresAt must be an RFC 3339 timestamp")
			return
		}
		if !at.After(now) {
			s.fail(w, r, http.StatusBadRequest, "expiresAt is in the past; that publishes nothing")
			return
		}
		// Something that ends before its review is reviewed by ending.
		if at.Before(review) {
			review = at
		}
		expires = at.UTC().Format(time.RFC3339)
	}

	e := gitops.Exposure{
		Install:      install,
		ExposureName: exposure,
		// From the token, never the body: an enablement that could name its
		// own owner records nobody.
		Owner:     c.meta.Subject,
		ReviewAt:  review.UTC().Format(time.RFC3339),
		ExpiresAt: expires,
		Reason:    strings.TrimSpace(body.Reason),
		Apex:      body.Apex,
	}
	if body.Apex {
		// Who acknowledged is the caller, from the token like the owner.
		e.ApexAcknowledgedBy = c.meta.Subject
	}
	res, err := s.cfg.Repo.PublishExposure(r.Context(), r.PathValue("t"), e, c.meta)
	if err != nil && errors.Is(err, gitops.ErrInvalidName) {
		// A name that is not a name is the caller's mistake, and the message
		// names the value rather than arriving as a rejected commit.
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil && errors.Is(err, gitops.ErrMainAddressNotAcknowledged) {
		s.fail(w, r, http.StatusBadRequest, mainAddressWarning)
		return
	}
	if err != nil && errors.Is(err, gitops.ErrMainAddressHeld) {
		// The message names the holder, which is what the approver needs.
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	s.written(w, r, res, err)
}

// mainAddressRefused is the answer to apex: true anywhere but the user
// tenant of a single-tenancy cluster.
const mainAddressRefused = "a website on the cluster's main address is for the user tenant of a single-tenancy cluster only. " +
	"On a multi-tenancy cluster the main address is the sign-in form, and the platform tenant's own page is published at install. " +
	"Nothing was changed"

// mainAddressWarning is the answer to apex: true without the acknowledgement:
// what the approver has to know before a website goes on the main address,
// and the field that says they do. The platform cannot check what a website
// loads, so the rule is the approver's to keep, and they are told it here, at
// the one place a website gets onto the address.
const mainAddressWarning = "a website on the cluster's main address needs your acknowledgement (\"acknowledgeMainAddressRule\": true). " +
	"Any script that runs in a page on the main address can set cookies for the whole domain, " +
	"and browsers send those cookies to the desktop, the consoles and sign-in as well. " +
	"Such a script cannot read anybody's session. " +
	"It can stop people from signing in until they clear their cookies, " +
	"and it can sign a person in to an account its author chose, without the person noticing, so that what they do there ends up in that account. " +
	"The platform cannot check what a website loads. " +
	"The rule: publish here only a site whose scripts your organisation itself controls -- " +
	"no third-party scripts (analytics, embeds, widgets, anything loaded from another host) and no pages uploaded by users. " +
	"If that is true of this site, send the request again with \"acknowledgeMainAddressRule\": true; your name and the time are recorded with the entry. " +
	"Nothing was changed"

// withdrawExposure takes one down.
func (s *Server) withdrawExposure(w http.ResponseWriter, r *http.Request, c call) {
	res, err := s.cfg.Repo.WithdrawExposure(
		r.Context(), r.PathValue("t"), r.PathValue("inst"), r.PathValue("name"), c.meta)
	s.written(w, r, res, err)
}
