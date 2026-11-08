/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package perimetertraffic

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gentian-org/gentian-os/internal/usage"
)

const schemaDDL = `
CREATE TABLE IF NOT EXISTS perimeter_traffic (
    observed_at TIMESTAMPTZ NOT NULL,
    component   TEXT        NOT NULL,
    exposure    TEXT        NOT NULL,
    path        TEXT        NOT NULL,
    requests    BIGINT      NOT NULL DEFAULT 0,
    c2xx        BIGINT      NOT NULL DEFAULT 0,
    c3xx        BIGINT      NOT NULL DEFAULT 0,
    c4xx        BIGINT      NOT NULL DEFAULT 0,
    c5xx        BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (observed_at, component, exposure, path)
);
CREATE INDEX IF NOT EXISTS ix_perimeter_traffic_observed_at
    ON perimeter_traffic (observed_at DESC);
`

// Row is the traffic one path of one entry carried between two readings.
type Row struct {
	ObservedAt time.Time `json:"observedAt,omitempty"`
	Component  string    `json:"component"`
	Exposure   string    `json:"exposure"`
	Path       string    `json:"path"`
	Counts
}

// Point is the traffic of all of a tenant's entries in one interval.
type Point struct {
	At time.Time `json:"at"`
	Counts
}

// Store is one tenant's traffic history, in the tenant's own database --
// the one its resource usage is kept in, and exported and purged with it.
type Store struct{ usage *usage.Store }

// NewStore wraps a tenant's usage store.
func NewStore(s *usage.Store) *Store { return &Store{usage: s} }

func (s *Store) conn(ctx context.Context) (*pgx.Conn, error) { return s.usage.Connect(ctx) }

// Record writes one reading's rows, creating the table where it is absent.
func (s *Store) Record(ctx context.Context, rows []Row) error {
	conn, err := s.conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, schemaDDL); err != nil {
		return fmt.Errorf("ensure traffic schema: %w", err)
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(`
            INSERT INTO perimeter_traffic (observed_at, component, exposure, path, requests, c2xx, c3xx, c4xx, c5xx)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
            ON CONFLICT DO NOTHING`,
			r.ObservedAt.UTC(), r.Component, r.Exposure, r.Path, r.Requests, r.C2xx, r.C3xx, r.C4xx, r.C5xx)
	}
	if err := conn.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("record traffic: %w", err)
	}
	return nil
}

// Prune deletes readings older than the cutoff.
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	conn, err := s.conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, schemaDDL); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, `DELETE FROM perimeter_traffic WHERE observed_at < $1`, before.UTC())
	return err
}

// Totals is each path's traffic over [from, to), busiest first.
func (s *Store) Totals(ctx context.Context, from, to time.Time) ([]Row, error) {
	conn, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, schemaDDL); err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, `
        SELECT component, exposure, path,
               SUM(requests), SUM(c2xx), SUM(c3xx), SUM(c4xx), SUM(c5xx)
        FROM perimeter_traffic
        WHERE observed_at >= $1 AND observed_at < $2
        GROUP BY component, exposure, path
        ORDER BY SUM(requests) DESC, component, exposure, path`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("query traffic totals: %w", err)
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Component, &r.Exposure, &r.Path, &r.Requests, &r.C2xx, &r.C3xx, &r.C4xx, &r.C5xx); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Series is the tenant's traffic over [from, to) in intervals of step.
func (s *Store) Series(ctx context.Context, from, to time.Time, step time.Duration) ([]Point, error) {
	conn, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, schemaDDL); err != nil {
		return nil, err
	}
	if step <= 0 {
		step = time.Hour
	}
	rows, err := conn.Query(ctx, `
        SELECT to_timestamp(floor(extract(epoch FROM observed_at) / $3) * $3) AS bucket,
               SUM(requests), SUM(c2xx), SUM(c3xx), SUM(c4xx), SUM(c5xx)
        FROM perimeter_traffic
        WHERE observed_at >= $1 AND observed_at < $2
        GROUP BY bucket ORDER BY bucket`, from.UTC(), to.UTC(), step.Seconds())
	if err != nil {
		return nil, fmt.Errorf("query traffic series: %w", err)
	}
	defer rows.Close()
	out := []Point{}
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.At, &p.Requests, &p.C2xx, &p.C3xx, &p.C4xx, &p.C5xx); err != nil {
			return nil, err
		}
		p.At = p.At.UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}
