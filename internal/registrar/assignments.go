/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gentian-org/gentian-os/internal/applifecycle"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
	"github.com/gentian-org/gentian-os/internal/guard"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// What a person holds in a tenant: their positions, and the roles beside.
//
// The registrar decides who may ask -- whoever manages the tenant's people,
// as for every other change to a person -- and that the person is one of
// the tenant's. It writes nothing itself: the rights store has one writer,
// the operator, and the registrar asks it (Rights). The operator in turn
// decides the act: that what is given is the tenant's own, and that a
// tenant keeps an administrator of its own.

// Rights is how the registrar has the operator give and take.
type Rights interface {
	// Give makes subject hold an assignment in tenant, for actor.
	Give(ctx context.Context, actor, tenant, subject string, a authz.Assignment) (changed bool, err error)
	// Take makes subject hold it no longer.
	Take(ctx context.Context, actor, tenant, subject string, a authz.Assignment) (changed bool, err error)
	// RemovePerson takes everything subject holds in tenant.
	RemovePerson(ctx context.Context, actor, tenant, subject string) (removed int, err error)
	// Assignments reads what subject was given in tenant.
	Assignments(ctx context.Context, tenant, subject string) ([]authz.Assignment, error)
	// Administrators reads the accounts that administer tenant as its own.
	Administrators(ctx context.Context, tenant string) ([]string, error)
	// People reads the accounts that are tenant's people: the holders of
	// its position user.
	People(ctx context.Context, tenant string) ([]string, error)
}

// ErrRefusedAssignment is an act the operator refused as not an assignment
// at all; its text says why and is the caller's to read.
var ErrRefusedAssignment = errors.New("not an assignment")

// operatorRights asks the operator's listener.
type operatorRights struct{ c *lifecycle.Client }

// NewOperatorRights returns Rights that asks the operator's listener at
// base, proving who asks with the token read from tokenFile on every call.
func NewOperatorRights(base, tokenFile string) Rights {
	return operatorRights{c: lifecycle.New(base, lifecycle.TokenFile(tokenFile))}
}

func assignmentPath(pattern, tenant, subject string) string {
	return strings.NewReplacer("{tenant}", url.PathEscape(tenant), "{subject}", url.PathEscape(subject)).Replace(pattern)
}

// refusal turns the operator's answer into the error a handler tells apart.
func refusal(status int, body []byte) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrRefusedAssignment, lifecycle.ErrorMessage(body))
	case http.StatusNotFound:
		return authz.ErrNoSuchThing
	case http.StatusConflict:
		return authz.ErrLastAdministrator
	}
	return fmt.Errorf("the operator answered %d", status)
}

func (o operatorRights) act(ctx context.Context, pattern, actor, tenant, subject string, body any) (applifecycle.AssignmentAnswer, error) {
	var answer applifecycle.AssignmentAnswer
	status, raw, err := o.c.Do(ctx, assignmentPath(pattern, tenant, subject), actor, body)
	if err != nil {
		return answer, err
	}
	if err := refusal(status, raw); err != nil {
		return answer, err
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return answer, fmt.Errorf("the operator's answer could not be read: %w", err)
	}
	return answer, nil
}

func (o operatorRights) Give(ctx context.Context, actor, tenant, subject string, a authz.Assignment) (bool, error) {
	answer, err := o.act(ctx, applifecycle.GiveAssignmentPath, actor, tenant, subject, a)
	return answer.Changed, err
}

func (o operatorRights) Take(ctx context.Context, actor, tenant, subject string, a authz.Assignment) (bool, error) {
	answer, err := o.act(ctx, applifecycle.TakeAssignmentPath, actor, tenant, subject, a)
	return answer.Changed, err
}

func (o operatorRights) RemovePerson(ctx context.Context, actor, tenant, subject string) (int, error) {
	answer, err := o.act(ctx, applifecycle.RemovePersonPath, actor, tenant, subject, struct{}{})
	return answer.Removed, err
}

func (o operatorRights) Assignments(ctx context.Context, tenant, subject string) ([]authz.Assignment, error) {
	status, raw, err := o.c.Get(ctx, assignmentPath(applifecycle.AssignmentsPath, tenant, subject), nil)
	if err != nil {
		return nil, err
	}
	if err := refusal(status, raw); err != nil {
		return nil, err
	}
	var answer applifecycle.AssignmentsAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("the operator's answer could not be read: %w", err)
	}
	return answer.Assignments, nil
}

func (o operatorRights) Administrators(ctx context.Context, tenant string) ([]string, error) {
	status, raw, err := o.c.Get(ctx, assignmentPath(applifecycle.AdministratorsPath, tenant, ""), nil)
	if err != nil {
		return nil, err
	}
	if err := refusal(status, raw); err != nil {
		return nil, err
	}
	var answer applifecycle.AdministratorsAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("the operator's answer could not be read: %w", err)
	}
	return answer.Subjects, nil
}

func (o operatorRights) People(ctx context.Context, tenant string) ([]string, error) {
	status, raw, err := o.c.Get(ctx, assignmentPath(applifecycle.PeopleSubjectsPath, tenant, ""), nil)
	if err != nil {
		return nil, err
	}
	if err := refusal(status, raw); err != nil {
		return nil, err
	}
	var answer applifecycle.PeopleSubjectsAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("the operator's answer could not be read: %w", err)
	}
	return answer.Subjects, nil
}

// rights is the way to the operator, or the answer that there is none.
func (s *Server) rights(w http.ResponseWriter, r *http.Request) Rights {
	if s.cfg.Rights == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "this registrar has no way to the rights store: positions and roles cannot be given here")
		return nil
	}
	return s.cfg.Rights
}

// rightsError answers an act the operator refused or could not do.
func (s *Server) rightsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrRefusedAssignment):
		s.fail(w, r, http.StatusBadRequest, err.Error())
	case errors.Is(err, authz.ErrNoSuchThing):
		s.fail(w, r, http.StatusNotFound, "this tenant has no such position, app or role")
	case errors.Is(err, authz.ErrLastAdministrator):
		s.fail(w, r, http.StatusConflict,
			"this is the tenant's last administrator of its own, who keeps the role: give it to somebody else first. Nothing was changed.")
	default:
		s.cfg.Log.ErrorContext(r.Context(), "the operator could not be asked about a person's rights",
			"request_id", guard.ID(r.Context()), "error", err.Error())
		s.fail(w, r, http.StatusBadGateway, "the rights store could not be reached. Nothing was changed.")
	}
}

// listAssignments answers what one of the tenant's people was given.
func (s *Server) listAssignments(w http.ResponseWriter, r *http.Request, _ call) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	rights := s.rights(w, r)
	if rights == nil {
		return
	}
	person, err := s.cfg.Identity.Person(identityContext(r), realm, r.PathValue("id"))
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	held, err := rights.Assignments(r.Context(), r.PathValue("t"), person.ID)
	if err != nil {
		s.rightsError(w, r, err)
		return
	}
	if held == nil {
		held = []authz.Assignment{}
	}
	s.json(w, http.StatusOK, map[string]any{"tenant": r.PathValue("t"), "person": person.ID, "assignments": held})
}

// assignmentBody is a position, or a role, and the person it is about.
type assignmentBody struct {
	Person   string `json:"person"`
	Position string `json:"position"`
	App      string `json:"app"`
	Role     string `json:"role"`
}

func (b assignmentBody) assignment() authz.Assignment {
	return authz.Assignment{Position: b.Position, App: b.App, Role: b.Role}
}

// what names an assignment for the record.
func what(a authz.Assignment) string {
	switch {
	case a.Position != "":
		return "position " + a.Position
	case a.App != "":
		return "role " + a.Role + " on " + a.App
	}
	return "role " + a.Role
}

// give gives one of the tenant's people a position, or a role beside their
// positions.
func (s *Server) give(w http.ResponseWriter, r *http.Request, c call) {
	s.assign(w, r, c, true)
}

// take takes one back.
func (s *Server) take(w http.ResponseWriter, r *http.Request, c call) {
	s.assign(w, r, c, false)
}

func (s *Server) assign(w http.ResponseWriter, r *http.Request, c call, give bool) {
	realm, ok := s.realmFor(w, r)
	if !ok {
		return
	}
	rights := s.rights(w, r)
	if rights == nil {
		return
	}
	var body assignmentBody
	if !s.decode(w, r, &body) {
		return
	}
	// The person is one of this tenant's: an id that is nobody's there is
	// given nothing.
	ctx := identityContext(r)
	person, err := s.cfg.Identity.Person(ctx, realm, body.Person)
	if err != nil {
		s.identityError(w, r, err)
		return
	}
	tenant, a := r.PathValue("t"), body.assignment()
	var changed bool
	verb := "take"
	if give {
		verb = "give"
		changed, err = rights.Give(r.Context(), c.subject, tenant, person.ID, a)
	} else {
		changed, err = rights.Take(r.Context(), c.subject, tenant, person.ID, a)
	}
	if err != nil {
		s.rightsError(w, r, err)
		return
	}
	out := map[string]any{"person": person.ID, "changed": changed, "holds": give}
	if a.Position != "" {
		out["position"] = a.Position
	} else {
		out["role"] = a.Role
		if a.App != "" {
			out["app"] = a.App
		}
	}
	if changed {
		s.recordIdentityAction(r, c, verb, realm, what(a)+" / "+person.Username)
		if give {
			// Whoever administers a tenant signs in with a second factor.
			// The role is given either way; that the factor could not be
			// asked for is said, and somebody can ask for it again.
			if required, err := s.requireSecondFactor(r, realm, tenant, person.ID); err != nil {
				out["warning"] = "the role was given, but a second factor could not be required of this person: " + err.Error()
			} else if required {
				out["totpRequired"] = true
			}
		}
	}
	s.json(w, http.StatusOK, out)
}

// requireSecondFactor asks an account for an authenticator when it
// administers the tenant: through the position every tenant has, a position
// that holds the role, one that includes such a position, or the role
// itself. It answers whether it asked.
func (s *Server) requireSecondFactor(r *http.Request, realm identity.Realm, tenant, subject string) (bool, error) {
	administrators, err := s.cfg.Rights.Administrators(r.Context(), tenant)
	if err != nil {
		return false, errors.New("the tenant's administrators could not be read")
	}
	for _, administrator := range administrators {
		if administrator != subject {
			continue
		}
		if err := s.cfg.Identity.RequireTOTP(identityContext(r), realm, subject, false, "", ""); err != nil {
			return false, errors.New("the sign-in service did not take it")
		}
		return true, nil
	}
	return false, nil
}
