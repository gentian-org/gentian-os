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
	"net/http"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// Who holds what in a tenant.
//
// The operator is the rights store's one writer, so giving a person a
// position or a role, and taking it, is done here and nowhere else. The
// registrar asks, for a person it has already found entitled to manage the
// tenant's people; the operator verifies no person, as on every route of
// this listener. What it does check is the act: that what is given is one
// of the tenant's own things, and that a tenant keeps an administrator of
// its own (internal/director/authz/assign.go).

// Paths of the assignment routes, for the registrar's client.
const (
	AssignmentsPath     = "/v1/tenants/{tenant}/people/{subject}/assignments"
	GiveAssignmentPath  = "/v1/tenants/{tenant}/people/{subject}/actions/give"
	TakeAssignmentPath  = "/v1/tenants/{tenant}/people/{subject}/actions/take"
	RemovePersonPath    = "/v1/tenants/{tenant}/people/{subject}/actions/remove-rights"
	AdministratorsPath  = "/v1/tenants/{tenant}/administrators"
	PeopleSubjectsPath  = "/v1/tenants/{tenant}/people-subjects"
	assignmentBodyLimit = 4 << 10
	// actorHeader names the person the caller verified and acts for. A
	// claim, recorded as one.
	actorHeader = "X-Gentian-Actor"
)

// AssignmentAnswer is what giving, taking and removing answer.
type AssignmentAnswer struct {
	// Changed is false when the store already was as asked.
	Changed bool `json:"changed"`
	// Removed is how many entries went, for a person's removal.
	Removed int `json:"removed,omitempty"`
}

// AssignmentsAnswer is what a person was given in a tenant.
type AssignmentsAnswer struct {
	Tenant      string             `json:"tenant"`
	Subject     string             `json:"subject"`
	Assignments []authz.Assignment `json:"assignments"`
}

// PeopleSubjectsAnswer is the accounts that are a tenant's people: the
// holders of its position user.
type PeopleSubjectsAnswer struct {
	Tenant   string   `json:"tenant"`
	Subjects []string `json:"subjects"`
}

// AdministratorsAnswer is the accounts that administer a tenant as its own.
type AdministratorsAnswer struct {
	Tenant   string   `json:"tenant"`
	Subjects []string `json:"subjects"`
}

func (h *HTTPServer) registerAssignmentRoutes(mux router) {
	mux.People("GET "+AssignmentsPath, h.handleAssignments)
	mux.People("POST "+GiveAssignmentPath, h.handleGive)
	mux.People("POST "+TakeAssignmentPath, h.handleTake)
	mux.People("POST "+RemovePersonPath, h.handleRemovePersonRights)
	mux.People("GET "+AdministratorsPath, h.handleAdministrators)
	mux.People("GET "+PeopleSubjectsPath, h.handlePeopleSubjects)
}

// rights is the store, or the answer that there is none.
func (h *HTTPServer) rights(w http.ResponseWriter) authz.Store {
	if h.Rights == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("this cluster's operator has no rights store to write to"))
		return nil
	}
	return h.Rights
}

// assignmentFailed answers a refused or failed act. What the caller got
// wrong is said to it; what the store did is logged and not passed on.
func assignmentFailed(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, authz.ErrInvalidID):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, authz.ErrNoSuchThing):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, authz.ErrLastAdministrator):
		writeErr(w, http.StatusConflict, err)
	default:
		ctrllog.FromContext(r.Context()).WithName("assignments").Error(err, "the rights store could not be read or written")
		writeErr(w, http.StatusBadGateway, errors.New("the rights store could not be read or written"))
	}
}

func assignmentOf(w http.ResponseWriter, r *http.Request) (authz.Assignment, bool) {
	var a authz.Assignment
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, assignmentBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("the body is an assignment: a position, or a role and the app it is held on"))
		return a, false
	}
	return a, true
}

func (h *HTTPServer) handleGive(w http.ResponseWriter, r *http.Request) {
	store := h.rights(w)
	if store == nil {
		return
	}
	a, ok := assignmentOf(w, r)
	if !ok {
		return
	}
	changed, err := authz.Give(r.Context(), store, r.PathValue("tenant"), r.PathValue("subject"), a)
	if err != nil {
		assignmentFailed(w, r, err)
		return
	}
	h.recordAssignment(r, "given", a, changed)
	h.rightsChanged(r, changed, false)
	writeJSON(w, http.StatusOK, AssignmentAnswer{Changed: changed})
}

func (h *HTTPServer) handleTake(w http.ResponseWriter, r *http.Request) {
	store := h.rights(w)
	if store == nil {
		return
	}
	a, ok := assignmentOf(w, r)
	if !ok {
		return
	}
	changed, err := authz.Take(r.Context(), store, r.PathValue("tenant"), r.PathValue("subject"), a)
	if err != nil {
		assignmentFailed(w, r, err)
		return
	}
	h.recordAssignment(r, "taken", a, changed)
	h.rightsChanged(r, changed, true)
	writeJSON(w, http.StatusOK, AssignmentAnswer{Changed: changed})
}

func (h *HTTPServer) handleRemovePersonRights(w http.ResponseWriter, r *http.Request) {
	store := h.rights(w)
	if store == nil {
		return
	}
	removed, err := authz.RemovePerson(r.Context(), store, r.PathValue("tenant"), r.PathValue("subject"))
	if err != nil {
		assignmentFailed(w, r, err)
		return
	}
	if removed > 0 {
		ctrllog.FromContext(r.Context()).WithName("assignments").Info("a person's rights removed",
			"tenant", r.PathValue("tenant"), "subject", r.PathValue("subject"), "entries", removed, "actor", r.Header.Get(actorHeader))
	}
	h.rightsChanged(r, removed > 0, true)
	writeJSON(w, http.StatusOK, AssignmentAnswer{Changed: removed > 0, Removed: removed})
}

func (h *HTTPServer) handleAssignments(w http.ResponseWriter, r *http.Request) {
	store := h.rights(w)
	if store == nil {
		return
	}
	tenant, subject := r.PathValue("tenant"), r.PathValue("subject")
	if !authz.ValidName(tenant) {
		writeErr(w, http.StatusBadRequest, errors.New("the tenant is not a name"))
		return
	}
	held, err := authz.Assignments(r.Context(), store, tenant, subject)
	if err != nil {
		assignmentFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, AssignmentsAnswer{Tenant: tenant, Subject: subject, Assignments: held})
}

func (h *HTTPServer) handleAdministrators(w http.ResponseWriter, r *http.Request) {
	store := h.rights(w)
	if store == nil {
		return
	}
	tenant := r.PathValue("tenant")
	if !authz.ValidName(tenant) {
		writeErr(w, http.StatusBadRequest, errors.New("the tenant is not a name"))
		return
	}
	subjects, err := authz.Administrators(r.Context(), store, tenant)
	if err != nil {
		assignmentFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, AdministratorsAnswer{Tenant: tenant, Subjects: subjects})
}

// recordAssignment logs an act that changed something, with the actor the
// caller names: the registrar's identity is the only one admitted here, and
// it names the person it verified.
func (h *HTTPServer) recordAssignment(r *http.Request, what string, a authz.Assignment, changed bool) {
	if !changed {
		return
	}
	ctrllog.FromContext(r.Context()).WithName("assignments").Info("an assignment "+what,
		"tenant", r.PathValue("tenant"), "subject", r.PathValue("subject"),
		"position", a.Position, "app", a.App, "role", a.Role, "actor", r.Header.Get(actorHeader))
}

// rightsChanged tells what follows from a change, when there was one.
func (h *HTTPServer) rightsChanged(r *http.Request, changed, taken bool) {
	if !changed || h.RightsChanged == nil {
		return
	}
	if err := h.RightsChanged(r.Context(), r.PathValue("tenant"), r.PathValue("subject"), taken); err != nil {
		ctrllog.FromContext(r.Context()).WithName("assignments").Error(err, "what follows from a change of rights was not done",
			"tenant", r.PathValue("tenant"), "subject", r.PathValue("subject"), "taken", taken)
	}
}

// handlePeopleSubjects answers who the tenant's people are, by subject: the
// holders of its position user. It is what confines the registrar to a
// tenant's own people in a realm several tenants share.
func (h *HTTPServer) handlePeopleSubjects(w http.ResponseWriter, r *http.Request) {
	store := h.rights(w)
	if store == nil {
		return
	}
	tenant := r.PathValue("tenant")
	if !authz.ValidName(tenant) {
		writeErr(w, http.StatusBadRequest, errors.New("the tenant is not a name"))
		return
	}
	subjects, err := authz.PositionHolders(r.Context(), store, tenant, authz.PositionUser)
	if err != nil {
		assignmentFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, PeopleSubjectsAnswer{Tenant: tenant, Subjects: subjects})
}
