/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package record is the director's durable record of WHO WAS ALLOWED to ask
// for a change to a person (S7A.17).
//
// Two halves, joined by a request id.
//
// WHAT CHANGED is Keycloak's own admin event, and Keycloak is the better
// witness for it: the event is written whether the change came through the
// director or through Keycloak's own console.
//
// WHO WAS ALLOWED TO ASK is this. Keycloak sees only the director's service
// account, so it cannot say which person asked or what permitted the call.
// That is the caller, the relation and the object the check was made against,
// and the request id both halves carry.
//
// Neither half is sufficient alone, which is why both exist.
//
// A database rather than a commit, and this is the one place the platform
// deliberately chooses that. Git gives every change an author, a time and a
// diff, and that is the standard the rest of the console is held to — but the
// thing that makes git right there, append-only history, is exactly what makes
// it wrong for a record about people. This one has a retention horizon, and a
// commit cannot.
package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Action is one identity change, as the director saw it.
type Action struct {
	// RequestID joins this to Keycloak's admin event for the same change.
	RequestID string
	// Action is the verb: invite, set-membership, send-password-reset.
	Action string
	// Realm and Tenant are what it was done in.
	Realm  string
	Tenant string
	// Target is who it was done to, by the name the realm knows.
	Target string
	// Principal is the Keycloak subject of the person who asked.
	Principal string
	// Decision is the relation and object that permitted the call, in the
	// form the commit trailer uses: "can_manage_users tenant:demo".
	Decision string
	At       time.Time
}

// Store writes and reads the record.
type Store struct {
	pool *pgxpool.Pool
	// retention bounds how long a row is kept. This is personal data: the
	// point of a database rather than a commit is that something eventually
	// removes it.
	retention time.Duration
}

// DefaultRetention is how long an authority record is kept when a deployment
// names no other horizon. Long enough to answer an audit of the previous
// financial year, short enough that it is not a permanent file on a person.
const DefaultRetention = 400 * 24 * time.Hour

const schema = `
CREATE TABLE IF NOT EXISTS identity_actions (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id  TEXT        NOT NULL,
    action      TEXT        NOT NULL,
    realm       TEXT        NOT NULL,
    tenant      TEXT        NOT NULL,
    target      TEXT        NOT NULL,
    principal   TEXT        NOT NULL,
    decision    TEXT        NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS identity_actions_request_id ON identity_actions (request_id);
CREATE INDEX IF NOT EXISTS identity_actions_tenant_time ON identity_actions (tenant, occurred_at DESC);
CREATE INDEX IF NOT EXISTS identity_actions_occurred_at ON identity_actions (occurred_at);
`

// Open connects and makes sure the table is there.
//
// The schema is created here rather than by a migration tool because it is one
// table that only this package writes: a migration framework would be more
// moving parts than the thing it migrates.
func Open(ctx context.Context, dsn string, retention time.Duration) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("record: no database url")
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("record: connect: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("record: schema: %w", err)
	}
	return &Store{pool: pool, retention: retention}, nil
}

// Close releases the pool.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Write records one action.
//
// The caller has already done the thing. A failure here is logged by the
// caller and does not undo it: refusing to have invited somebody because the
// record could not be written would be a worse outcome than a gap in the
// record, and the gap is visible because Keycloak's own event has no partner.
func (s *Store) Write(ctx context.Context, a Action) error {
	if s == nil || s.pool == nil {
		return nil
	}
	at := a.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO identity_actions
		    (request_id, action, realm, tenant, target, principal, decision, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		a.RequestID, a.Action, a.Realm, a.Tenant, a.Target, a.Principal, a.Decision, at)
	if err != nil {
		return fmt.Errorf("record: write: %w", err)
	}
	return nil
}

// ByTenant reads a tenant's record, newest first, for the console's audit
// view. limit is bounded so a screen cannot ask for the whole table.
func (s *Store) ByTenant(ctx context.Context, tenant string, limit int) ([]Action, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("record: unavailable")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT request_id, action, realm, tenant, target, principal, decision, occurred_at
		  FROM identity_actions
		 WHERE tenant = $1
		 ORDER BY occurred_at DESC
		 LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, fmt.Errorf("record: read: %w", err)
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		if err := rows.Scan(&a.RequestID, &a.Action, &a.Realm, &a.Tenant,
			&a.Target, &a.Principal, &a.Decision, &a.At); err != nil {
			return nil, fmt.Errorf("record: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Prune removes what is past the retention horizon and answers how many rows
// went. Personal data does not keep itself; something has to delete it, and
// this is that something.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM identity_actions WHERE occurred_at < $1`,
		time.Now().UTC().Add(-s.retention))
	if err != nil {
		return 0, fmt.Errorf("record: prune: %w", err)
	}
	return tag.RowsAffected(), nil
}
