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
	"errors"
	"net/http"
	"net/url"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// What became of a tenant's apps, and the two things done to one of them that
// are not a change to what the tenant is.
//
// GET /v1/tenants/{t}/apps answers from git: what the tenant is meant to have.
// What the cluster made of it -- still coming up, running, broken -- is the
// operator's to say and the usher's to relay; it is not served here.
//
// Purging and provisioning are actions. Neither is desired state: a purge
// deletes what an app that is already gone left behind, and provisioning
// puts the people who are members today into a group. There is nothing to
// commit, so the person is named on the request instead of on a commit.

func appsPath(r *http.Request, suffix string) string {
	return "/v1/tenants/" + url.PathEscape(r.PathValue("t")) + suffix
}

// appAction relays one action about one profile.
func (s *Server) appAction(w http.ResponseWriter, r *http.Request, c call, action string) {
	var body struct {
		Profile string `json:"profile"`
	}
	if err := decode(r, &body); err != nil || !gitops.ValidName(body.Profile) {
		s.fail(w, r, http.StatusBadRequest, `body must be {"profile": "<name>"}`)
		return
	}
	status, answer, err := s.cfg.Lifecycle.Do(r.Context(),
		appsPath(r, "/actions/"+action), c.meta.ActorName(), map[string]string{"profile": body.Profile})
	if errors.Is(err, context.DeadlineExceeded) {
		// Not "the operator did not answer": it was asked, and what it has
		// done by now is not known here.
		s.cfg.Log.ErrorContext(r.Context(), "an action outlasted its relay", "action", action,
			"request_id", reqID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusGatewayTimeout, "the operator had not answered "+action+
			" when the director stopped waiting; what it did before then is not known here. Ask again: the action is safe to repeat")
		return
	}
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	s.started(w, r, status, answer)
}

// purgeApp destroys the data an uninstalled app left behind. The operator
// refuses while the tenant still has the app, so this cannot take data from
// something that is running.
//
// One request, answered when the purge is over: the operator does all of it
// before it answers and continues nothing afterwards. So this relay waits
// lifecycle.PurgeDeadline for it, which is longer than the operator gives
// itself, and a purge that did not complete comes back as the operator's own
// refusal -- which step failed, what was already destroyed, and to retry --
// not as a relay that gave up half-way.
func (s *Server) purgeApp(w http.ResponseWriter, r *http.Request, c call) {
	ctx, cancel := lifecycle.Patient(r.Context(), lifecycle.PurgeDeadline)
	defer cancel()
	s.appAction(w, r.WithContext(ctx), c, "purge-app")
}

// provisionApp grants an installed app to everybody who is a member now.
func (s *Server) provisionApp(w http.ResponseWriter, r *http.Request, c call) {
	s.appAction(w, r, c, "provision-app")
}
