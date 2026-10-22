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
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gentian-org/gentian-os/internal/registrar/identity"
	"github.com/gentian-org/gentian-os/internal/registrar/record"
)

// Inviting the administrator of a tenant that does not exist yet.
//
// A new tenant takes minutes to exist: its manifest is synced, its realm is
// made, its administrator account is created. Whoever created the tenant
// knows where the administrator's invitation should go at the moment they
// create it, and should not have to keep a page open until then. So the
// request is kept here, and the registrar sends the invitation itself once
// the activation is possible -- the same activation the route beside it
// performs while somebody waits (handOverAdmin), mailed and never shown.
//
// What is kept is a person's address, and it is kept in the registrar's own
// database and nowhere else: a tenant's manifest names no person, and neither
// does a log line here. The row is deleted when the invitation is sent, when
// the realm could not mail it, when the realm refused, and when the request
// expires without its tenant becoming ready. What remains is the outcome in
// the record, which names the account and not the address.
//
// Only with an address. Without one the link has to be shown to somebody,
// once, and there is nobody here to show it to; that stays with the route
// that is asked while somebody waits.

// DefaultInvitationExpiry is how long a request waits for its tenant when a
// deployment names no other time. A tenant that is not ready after a day is
// not about to be, and an address is not kept for one that never is.
const DefaultInvitationExpiry = 24 * time.Hour

// defaultInvitationInterval is how often the waiting requests are tried.
const defaultInvitationInterval = 30 * time.Second

// Invitations is where the requests are kept: record.Store, and an interface
// so the routes can be tested without a database.
type Invitations interface {
	// RequestInvitation keeps a request in place of the one its tenant had.
	RequestInvitation(ctx context.Context, inv record.Invitation) error
	// PendingInvitations is every request that is waiting.
	PendingInvitations(ctx context.Context) ([]record.Invitation, error)
	// AttemptInvitation holds one tenant's request while attempt tries it.
	// An outcome deletes the request and is recorded; nil leaves it waiting.
	AttemptInvitation(ctx context.Context, tenant string, attempt func(record.Invitation) *record.Action) error
	// InvitationState is whether a tenant's request is waiting, and
	// otherwise what became of the last.
	InvitationState(ctx context.Context, tenant string) (record.InvitationState, error)
}

// requestAdminInvitation keeps a request to invite a tenant's administrator
// and answers at once. The tenant need not exist.
func (s *Server) requestAdminInvitation(w http.ResponseWriter, r *http.Request, c call) {
	var body struct {
		RecoveryEmail string `json:"recoveryEmail"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	tenant := r.PathValue("t")
	if !dnsLabel.MatchString(tenant) {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.RecoveryEmail))
	if email == "" {
		s.fail(w, r, http.StatusBadRequest,
			"an invitation is mailed, so it needs an address: without one, activate the administrator once the tenant is ready")
		return
	}
	if !identity.LooksLikeAddress(email) {
		s.fail(w, r, http.StatusBadRequest, "that is not an address")
		return
	}
	if s.cfg.Invitations == nil {
		s.fail(w, r, http.StatusServiceUnavailable,
			"this registrar has no database, so it cannot keep an invitation: activate the administrator once the tenant is ready")
		return
	}
	ctx := r.Context()
	now := s.cfg.Now().UTC()
	inv := record.Invitation{
		Tenant:      tenant,
		Email:       email,
		Principal:   c.subject,
		Decision:    c.decision,
		RequestID:   reqID(ctx),
		RequestedAt: now,
		ExpiresAt:   now.Add(s.cfg.InvitationExpiry),
	}
	// Not the request's context: a request that was kept must not be lost
	// to the caller hanging up, nor half kept.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.cfg.Invitations.RequestInvitation(writeCtx, inv); err != nil {
		s.cfg.Log.ErrorContext(ctx, "an administrator's invitation could not be kept",
			"request_id", reqID(ctx), "tenant", tenant, "error", scrub(err.Error(), email))
		s.fail(w, r, http.StatusServiceUnavailable, "the invitation could not be kept")
		return
	}
	s.logInvitation(ctx, record.Action{
		RequestID: inv.RequestID, Action: record.ActionInvitationRequested,
		Tenant: tenant, Principal: inv.Principal, Decision: inv.Decision,
	})
	// A tenant that is ready already is not kept waiting for the next round.
	select {
	case s.retry <- struct{}{}:
	default:
	}
	s.json(w, http.StatusAccepted, map[string]any{
		"tenant":    tenant,
		"state":     record.InvitationPending,
		"expiresAt": inv.ExpiresAt.Unix(),
	})
}

// adminInvitation answers whether a tenant's invitation is waiting, and
// otherwise what became of the last one. Never where it goes.
func (s *Server) adminInvitation(w http.ResponseWriter, r *http.Request, _ call) {
	tenant := r.PathValue("t")
	if !dnsLabel.MatchString(tenant) {
		s.fail(w, r, http.StatusBadRequest, "invalid name")
		return
	}
	if s.cfg.Invitations == nil {
		s.json(w, http.StatusOK, map[string]any{"tenant": tenant, "state": record.InvitationNone})
		return
	}
	state, err := s.cfg.Invitations.InvitationState(r.Context(), tenant)
	if err != nil {
		s.cfg.Log.ErrorContext(r.Context(), "the state of an administrator's invitation could not be read",
			"request_id", reqID(r.Context()), "tenant", tenant, "error", err.Error())
		s.fail(w, r, http.StatusServiceUnavailable, "the invitation's state could not be read")
		return
	}
	out := map[string]any{"tenant": tenant, "state": state.State}
	if !state.At.IsZero() {
		out["at"] = state.At.Unix()
	}
	if !state.ExpiresAt.IsZero() {
		out["expiresAt"] = state.ExpiresAt.Unix()
	}
	if state.Reason != "" {
		out["reason"] = state.Reason
	}
	s.json(w, http.StatusOK, out)
}

// pendingAdminInvitations answers which tenants have an invitation waiting.
func (s *Server) pendingAdminInvitations(w http.ResponseWriter, r *http.Request, _ call) {
	pending := []map[string]any{}
	if s.cfg.Invitations != nil {
		waiting, err := s.cfg.Invitations.PendingInvitations(r.Context())
		if err != nil {
			s.cfg.Log.ErrorContext(r.Context(), "the waiting invitations could not be read",
				"request_id", reqID(r.Context()), "error", err.Error())
			s.fail(w, r, http.StatusServiceUnavailable, "the waiting invitations could not be read")
			return
		}
		for _, inv := range waiting {
			pending = append(pending, map[string]any{
				"tenant":      inv.Tenant,
				"requestedAt": inv.RequestedAt.Unix(),
				"expiresAt":   inv.ExpiresAt.Unix(),
			})
		}
	}
	s.json(w, http.StatusOK, map[string]any{"pending": pending})
}

// sendInvitations tries the waiting requests, at once and then at an
// interval, and whenever a new one is kept, until ctx is done.
//
// In the process and not a CronJob, for the reason the record is pruned
// here: the requests are in a database only this process has a credential
// for, and sending one takes the Keycloak credential only this process has.
func (s *Server) sendInvitations(ctx context.Context) {
	tick := time.NewTicker(s.cfg.InvitationInterval)
	defer tick.Stop()
	for {
		s.SendInvitations(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.retry:
		}
	}
}

// SendInvitations tries every waiting request once: it sends the ones whose
// tenant is ready, gives up the ones that expired, and leaves the rest.
func (s *Server) SendInvitations(ctx context.Context) {
	if s.cfg.Invitations == nil {
		return
	}
	waiting, err := s.cfg.Invitations.PendingInvitations(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.cfg.Log.ErrorContext(ctx, "the waiting invitations could not be read", "error", err.Error())
		}
		return
	}
	for _, inv := range waiting {
		if ctx.Err() != nil {
			return
		}
		var outcome *record.Action
		err := s.cfg.Invitations.AttemptInvitation(ctx, inv.Tenant, func(held record.Invitation) *record.Action {
			outcome = s.sendInvitation(ctx, held)
			return outcome
		})
		if err != nil {
			// What was done is done; the request is still there and is tried
			// again, which for one that was sent means a second mail.
			s.cfg.Log.ErrorContext(ctx, "an administrator's invitation could not be settled",
				"request_id", inv.RequestID, "tenant", inv.Tenant, "error", scrub(err.Error(), inv.Email))
			continue
		}
		if outcome != nil {
			s.logInvitation(ctx, *outcome)
		}
	}
}

// sendInvitation is one attempt at one request, and its outcome: nil while
// the tenant is not ready, and otherwise what is recorded in the request's
// place.
func (s *Server) sendInvitation(ctx context.Context, inv record.Invitation) *record.Action {
	now := s.cfg.Now().UTC()
	outcome := &record.Action{
		RequestID: inv.RequestID,
		Tenant:    inv.Tenant,
		Principal: inv.Principal,
		Decision:  inv.Decision,
		At:        now,
	}
	if !now.Before(inv.ExpiresAt) {
		outcome.Action = record.ActionInvitationExpired
		return outcome
	}
	// What the route does for a caller, for the person who asked: their
	// request id on Keycloak's event. And nobody to show a link to.
	actx := identity.WithMailOnly(identity.WithRequestID(ctx, inv.RequestID))
	actx, cancel := context.WithTimeout(actx, time.Minute)
	defer cancel()
	handed, err := s.handOverAdmin(actx, inv.Tenant, inv.Email)
	outcome.Realm = handed.realm.Name()
	outcome.Target = handed.username
	var waiting *notReady
	switch {
	case err == nil:
	case errors.Is(err, ErrTenantNotFound), errors.As(err, &waiting):
		return nil
	case errors.Is(err, identity.ErrProtected):
		// Refused for whoever asks, and so for as often as it is asked: the
		// account is one the registrar does not hand over.
		outcome.Action = record.ActionInvitationRefused
		outcome.Detail = scrub(err.Error(), inv.Email)
		return outcome
	default:
		// The cluster or the realm did not answer. It may the next time.
		s.cfg.Log.WarnContext(ctx, "an administrator's invitation could not be sent yet",
			"request_id", inv.RequestID, "tenant", inv.Tenant, "error", scrub(err.Error(), inv.Email))
		return nil
	}
	if handed.activation.Mailed {
		outcome.Action = record.ActionInvitationSent
		return outcome
	}
	// The realm cannot send mail. No link was made (identity.WithMailOnly);
	// the address is on the account as its recovery address, and the
	// administrator is activated by somebody who is there to be shown one.
	outcome.Action = record.ActionInvitationNotMailed
	outcome.Detail = scrub(handed.activation.MailError, inv.Email)
	return outcome
}

// logInvitation is the log line for what became of a request: the line every
// identity action leaves, with the account and without the address.
func (s *Server) logInvitation(ctx context.Context, a record.Action) {
	s.cfg.Log.InfoContext(ctx, "identity action",
		"request_id", a.RequestID,
		"action", a.Action,
		"realm", a.Realm,
		"tenant", a.Tenant,
		"target", a.Target,
		"principal", a.Principal,
		"decision", a.Decision,
		"detail", a.Detail,
	)
}

// scrub takes an address out of a text that is about to be logged or
// recorded. Nothing here puts it in one; an error from further down might.
func scrub(text, address string) string {
	if address == "" {
		return text
	}
	lower := strings.ToLower(text)
	address = strings.ToLower(address)
	if len(lower) != len(text) {
		// Lowering moved the bytes, so the address cannot be cut out by
		// position: the text goes rather than the address stay.
		if strings.Contains(lower, address) {
			return "<a text that named the address>"
		}
		return text
	}
	var out strings.Builder
	for {
		i := strings.Index(lower, address)
		if i < 0 {
			break
		}
		out.WriteString(text[:i])
		out.WriteString("<address>")
		text, lower = text[i+len(address):], lower[i+len(address):]
	}
	out.WriteString(text)
	return out.String()
}
