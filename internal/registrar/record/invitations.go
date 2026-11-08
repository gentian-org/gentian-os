/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// A tenant's administrator is invited by mail once the tenant exists, and a
// new tenant takes minutes to exist. The request is kept here in between, so
// that it does not depend on a page somebody has to leave open.
//
// The row holds a person's address, which is why it is in this database and
// nowhere else: not in git, not on the Tenant, not in a log. It is the only
// row here that does, and it does not outlive its request: it is deleted when
// the invitation is sent, when it could not be, and when the request expires.
// What stays is the outcome, in the record of identity actions, without the
// address.

// Invitation is one request for a tenant's administrator to be invited.
type Invitation struct {
	// Tenant is whose administrator. There is one request per tenant.
	Tenant string
	// Email is where the invitation goes, and what becomes the account's
	// recovery address.
	Email string
	// Principal is the Keycloak subject of the person who asked, Decision
	// what permitted them to, and RequestID the id of their request: what an
	// Action carries, kept until there is an outcome to record.
	Principal string
	Decision  string
	RequestID string
	// RequestedAt is when it was asked, and ExpiresAt when it is given up.
	RequestedAt time.Time
	ExpiresAt   time.Time
}

// What becomes of a request, as actions in the record.
const (
	ActionInvitationRequested = "request-admin-invitation"
	ActionInvitationSent      = "admin-invitation-sent"
	ActionInvitationExpired   = "admin-invitation-expired"
	ActionInvitationNotMailed = "admin-invitation-not-mailed"
	ActionInvitationRefused   = "admin-invitation-refused"
	// ActionActivateAdmin is the activation somebody issued by hand, which
	// supersedes whatever became of a request before it.
	ActionActivateAdmin = "activate-admin"
)

// The states a tenant's invitation is in.
const (
	InvitationNone      = "none"
	InvitationPending   = "pending"
	InvitationSent      = "sent"
	InvitationExpired   = "expired"
	InvitationNotMailed = "not-mailed"
	InvitationRefused   = "refused"
)

// InvitationState is what is known about a tenant's invitation: whether one
// is waiting, and otherwise what became of the last. It names no address.
type InvitationState struct {
	State string
	// At is when it was asked while it is pending, and when it ended after.
	At time.Time
	// ExpiresAt is set while it is pending.
	ExpiresAt time.Time
	// Reason is why it was not mailed or was refused.
	Reason string
}

// OutcomeState is the state an action in the record leaves a tenant's
// invitation in, and whether the action is about the invitation at all.
func OutcomeState(action string) (string, bool) {
	switch action {
	case ActionInvitationSent:
		return InvitationSent, true
	case ActionInvitationExpired:
		return InvitationExpired, true
	case ActionInvitationNotMailed:
		return InvitationNotMailed, true
	case ActionInvitationRefused:
		return InvitationRefused, true
	case ActionActivateAdmin:
		return InvitationNone, true
	}
	return "", false
}

var errUnavailable = errors.New("record: unavailable")

// RequestInvitation keeps a request, in place of the one the tenant had, and
// records that it was asked.
func (s *Store) RequestInvitation(ctx context.Context, inv Invitation) error {
	if s == nil || s.pool == nil {
		return errUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record: invitation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO pending_admin_invitations
		    (tenant, email, principal, decision, request_id, requested_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant) DO UPDATE SET
		    email = EXCLUDED.email, principal = EXCLUDED.principal, decision = EXCLUDED.decision,
		    request_id = EXCLUDED.request_id, requested_at = EXCLUDED.requested_at, expires_at = EXCLUDED.expires_at`,
		inv.Tenant, inv.Email, inv.Principal, inv.Decision, inv.RequestID, inv.RequestedAt, inv.ExpiresAt); err != nil {
		return fmt.Errorf("record: invitation: %w", err)
	}
	if _, err := tx.Exec(ctx, insertAction,
		inv.RequestID, ActionInvitationRequested, "", inv.Tenant, "", inv.Principal, inv.Decision, "", inv.RequestedAt); err != nil {
		return fmt.Errorf("record: invitation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record: invitation: %w", err)
	}
	return nil
}

// PendingInvitations is every request that is waiting, oldest first.
func (s *Store) PendingInvitations(ctx context.Context) ([]Invitation, error) {
	if s == nil || s.pool == nil {
		return nil, errUnavailable
	}
	rows, err := s.pool.Query(ctx, `
		SELECT tenant, email, principal, decision, request_id, requested_at, expires_at
		  FROM pending_admin_invitations
		 ORDER BY requested_at, tenant`)
	if err != nil {
		return nil, fmt.Errorf("record: invitations: %w", err)
	}
	defer rows.Close()
	var out []Invitation
	for rows.Next() {
		var i Invitation
		if err := rows.Scan(&i.Tenant, &i.Email, &i.Principal, &i.Decision, &i.RequestID, &i.RequestedAt, &i.ExpiresAt); err != nil {
			return nil, fmt.Errorf("record: invitations: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// AttemptInvitation holds a tenant's request while attempt tries to carry it
// out. An outcome ends it: the request is deleted and the outcome recorded,
// together. No outcome leaves it waiting.
//
// Held means no other registrar attempts the same request meanwhile -- two
// replicas are the same registrar, and the invitation is one mail. A request
// that is held elsewhere, or is no longer there, is not attempted.
func (s *Store) AttemptInvitation(ctx context.Context, tenant string, attempt func(Invitation) *Action) error {
	if s == nil || s.pool == nil {
		return errUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record: invitation: %w", err)
	}
	// Not ctx: what attempt did is done, and the request must not stay
	// behind to be done again because the caller was stopped meanwhile.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var i Invitation
	err = tx.QueryRow(ctx, `
		SELECT tenant, email, principal, decision, request_id, requested_at, expires_at
		  FROM pending_admin_invitations
		 WHERE tenant = $1
		   FOR UPDATE SKIP LOCKED`, tenant).
		Scan(&i.Tenant, &i.Email, &i.Principal, &i.Decision, &i.RequestID, &i.RequestedAt, &i.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record: invitation: %w", err)
	}
	outcome := attempt(i)
	if outcome == nil {
		return nil
	}
	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := tx.Exec(done, `DELETE FROM pending_admin_invitations WHERE tenant = $1`, tenant); err != nil {
		return fmt.Errorf("record: invitation: %w", err)
	}
	at := outcome.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if _, err := tx.Exec(done, insertAction,
		outcome.RequestID, outcome.Action, outcome.Realm, outcome.Tenant, outcome.Target,
		outcome.Principal, outcome.Decision, outcome.Detail, at); err != nil {
		return fmt.Errorf("record: invitation: %w", err)
	}
	if err := tx.Commit(done); err != nil {
		return fmt.Errorf("record: invitation: %w", err)
	}
	return nil
}

// InvitationState answers whether a tenant's invitation is waiting, and
// otherwise what became of the last one.
func (s *Store) InvitationState(ctx context.Context, tenant string) (InvitationState, error) {
	if s == nil || s.pool == nil {
		return InvitationState{}, errUnavailable
	}
	var st InvitationState
	err := s.pool.QueryRow(ctx, `
		SELECT requested_at, expires_at FROM pending_admin_invitations WHERE tenant = $1`, tenant).
		Scan(&st.At, &st.ExpiresAt)
	if err == nil {
		st.State = InvitationPending
		return st, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return InvitationState{}, fmt.Errorf("record: invitation: %w", err)
	}
	var action string
	err = s.pool.QueryRow(ctx, `
		SELECT action, detail, occurred_at
		  FROM identity_actions
		 WHERE tenant = $1 AND action = ANY($2)
		 ORDER BY occurred_at DESC, id DESC
		 LIMIT 1`, tenant, []string{
		ActionInvitationSent, ActionInvitationExpired, ActionInvitationNotMailed,
		ActionInvitationRefused, ActionActivateAdmin,
	}).Scan(&action, &st.Reason, &st.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvitationState{State: InvitationNone}, nil
	}
	if err != nil {
		return InvitationState{}, fmt.Errorf("record: invitation: %w", err)
	}
	st.State, _ = OutcomeState(action)
	if st.State == InvitationNone {
		return InvitationState{State: InvitationNone}, nil
	}
	return st, nil
}
