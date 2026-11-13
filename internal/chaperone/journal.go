/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// journalSchema is the journal's one table. BIGINT for the time: seconds
// since 1970 outlive an INTEGER.
const journalSchema = `
CREATE TABLE IF NOT EXISTS journal (
	id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	at               BIGINT NOT NULL,
	tenant           TEXT NOT NULL,
	person           TEXT NOT NULL,
	engine           TEXT NOT NULL,
	fingerprint      TEXT NOT NULL,
	run              TEXT NOT NULL,
	mandate          TEXT NOT NULL,
	tool             TEXT NOT NULL,
	cls              TEXT,
	stage            TEXT,
	decision         TEXT NOT NULL,
	reason           TEXT NOT NULL DEFAULT '',
	outcome          TEXT,
	arguments_digest TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS journal_run ON journal (run);
CREATE INDEX IF NOT EXISTS journal_person_at ON journal (person, at);
`

// SQLJournal is the journal in the chaperone's own database.
type SQLJournal struct{ pool *pgxpool.Pool }

// OpenJournal connects and makes the table if it is not there.
func OpenJournal(ctx context.Context, dsn string) (*SQLJournal, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, journalSchema); err != nil {
		pool.Close()
		return nil, err
	}
	return &SQLJournal{pool: pool}, nil
}

// Close releases the connections.
func (j *SQLJournal) Close() { j.pool.Close() }

// Ping reports whether the database answers.
func (j *SQLJournal) Ping(ctx context.Context) error { return j.pool.Ping(ctx) }

func (j *SQLJournal) Add(ctx context.Context, l Line) error {
	_, err := j.pool.Exec(ctx, `
INSERT INTO journal (at, tenant, person, engine, fingerprint, run, mandate, tool, cls, stage, decision, reason, outcome, arguments_digest)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), $11, $12, NULLIF($13, ''), $14)`,
		l.At, l.Tenant, l.Person, l.Engine, l.Fingerprint, l.Run, l.Mandate, l.Tool, l.Class, l.Stage,
		l.Decision, l.Reason, l.Outcome, l.ArgumentsDigest)
	return err
}

// MemoryJournal keeps lines in memory, for tests.
type MemoryJournal struct {
	mu    sync.Mutex
	lines []Line
}

func (j *MemoryJournal) Add(_ context.Context, l Line) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, l)
	return nil
}

// Lines are the lines added so far.
func (j *MemoryJournal) Lines() []Line {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Line(nil), j.lines...)
}
