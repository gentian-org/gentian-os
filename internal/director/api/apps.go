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
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/gitops"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/security"
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

// requestPrivilege asks the security officer for a privilege the app waits
// for and the tenant cannot approve itself: a pod-security waiver or a
// cluster role. The request is recorded by the operator on the Component
// that waits -- who asked, when, and a note -- and listed to the officer at
// /v1/clusters/{c}/approvals (the usher's). The approval stays what it was:
// PUT /v1/tenants/{t}/privileges/{inst}/{kind}/{name}, decided by the kind.
//
// An action, not a commit: asking changes nothing the tenant is meant to
// have, and the answer, when it comes, is the commit. Whoever may install
// may ask, as whoever may install may purge.
func (s *Server) requestPrivilege(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Privilege string `json:"privilege"`
		Note      string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"privilege": "<kind>/<name>", "note": "..."}`)
		return
	}
	if _, ok := s.pendingPrivilegeRef(w, r, body.Privilege); !ok {
		return
	}
	status, answer, err := s.cfg.Lifecycle.Do(r.Context(), appsPath(r, "/actions/request-privilege"), c.meta.ActorName(),
		map[string]string{"profile": r.PathValue("p"), "privilege": body.Privilege, "note": strings.TrimSpace(body.Note)})
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	s.started(w, r, status, answer)
}

// pendingPrivilegeRef checks the privilege an action on {p} names, and
// answers the call when it is not one. It returns the kind.
func (s *Server) pendingPrivilegeRef(w http.ResponseWriter, r *http.Request, privilege string) (string, bool) {
	kind, name := security.SplitPrivilegeRef(privilege)
	switch kind {
	case security.PrivilegePodSecurity, security.PrivilegeEgress, security.PrivilegeClusterRoles:
	default:
		s.fail(w, r, http.StatusBadRequest, "the privilege is <kind>/<name>, the kind one of egress, podSecurity or clusterRoles")
		return "", false
	}
	if !gitops.ValidName(name) || !gitops.ValidName(r.PathValue("p")) {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return "", false
	}
	return kind, true
}

// declinePrivilege is the approver's no. Whoever may approve a privilege of
// this kind may decline it -- mayApprove, the same check as the grant's, so
// a tenant administrator cannot decline a waiver any more than approve one
// -- and nobody else. It is an action and not a commit: a decline changes
// nothing the platform gives the app. The privilege stays pending, the
// install keeps waiting, and the operator records on the Component who
// declined, when and why (privilege_requests.go), which the tenant's console
// and the install step show. A later approval is the commit it always was,
// and clears the decline; a new request clears it too.
func (s *Server) declinePrivilege(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		Privilege string `json:"privilege"`
		Reason    string `json:"reason"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, http.StatusBadRequest, `body must be {"privilege": "<kind>/<name>", "reason": "..."}`)
		return
	}
	kind, ok := s.pendingPrivilegeRef(w, r, body.Privilege)
	if !ok {
		return
	}
	if _, ok := s.mayApprove(w, r, &c, kind); !ok {
		return
	}
	// As much as a grant asks for, and for the same reader: the person who
	// asked, and whoever reviews the answer later.
	if len(strings.TrimSpace(body.Reason)) < 10 {
		s.fail(w, r, http.StatusBadRequest, "a decline needs a reason of at least ten characters, in your own words")
		return
	}
	status, answer, err := s.cfg.Lifecycle.Do(r.Context(), appsPath(r, "/actions/decline-privilege"), c.meta.ActorName(),
		map[string]string{"profile": r.PathValue("p"), "privilege": body.Privilege, "reason": strings.TrimSpace(body.Reason)})
	if err != nil {
		s.lifecycleError(w, r, err)
		return
	}
	s.started(w, r, status, answer)
}
