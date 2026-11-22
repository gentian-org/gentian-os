/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package homesync

import (
	"context"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pair is a record of an app and the home's entry it is kept equal to, as
// both were when they last agreed: which is what tells a change from a
// deletion on either side.
type Pair struct {
	Person string
	App    string
	Kind   Kind
	AppID  string
	Stamp  string // the record's stamp when they last agreed
	Href   string
	ETag   string // the entry's version when they last agreed
}

// Store keeps the pairs.
type Store interface {
	Pairs(ctx context.Context, person, app string, kind Kind) ([]Pair, error)
	Save(ctx context.Context, p Pair) error
	Remove(ctx context.Context, p Pair) error
}

// Memory is a Store in the process, for tests.
type Memory struct {
	mu    sync.Mutex
	pairs map[[4]string]Pair
}

func memKey(p Pair) [4]string { return [4]string{p.Person, p.App, string(p.Kind), p.AppID} }

func (m *Memory) Pairs(_ context.Context, person, app string, kind Kind) ([]Pair, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Pair
	for _, p := range m.pairs {
		if p.Person == person && p.App == app && p.Kind == kind {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AppID < out[j].AppID })
	return out, nil
}

func (m *Memory) Save(_ context.Context, p Pair) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pairs == nil {
		m.pairs = map[[4]string]Pair{}
	}
	m.pairs[memKey(p)] = p
	return nil
}

func (m *Memory) Remove(_ context.Context, p Pair) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pairs, memKey(p))
	return nil
}

// Postgres is the Store in the service's database.
type Postgres struct {
	Pool *pgxpool.Pool
}

const schema = `CREATE TABLE IF NOT EXISTS homesync_pairs (
	person text NOT NULL,
	app    text NOT NULL,
	kind   text NOT NULL,
	app_id text NOT NULL,
	stamp  text NOT NULL,
	href   text NOT NULL,
	etag   text NOT NULL,
	PRIMARY KEY (person, app, kind, app_id)
)`

// OpenPostgres connects and makes the table where it is not there.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{Pool: pool}, nil
}

func (s *Postgres) Pairs(ctx context.Context, person, app string, kind Kind) ([]Pair, error) {
	rows, err := s.Pool.Query(ctx, `SELECT app_id, stamp, href, etag FROM homesync_pairs
		WHERE person = $1 AND app = $2 AND kind = $3 ORDER BY app_id`, person, app, string(kind))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pair
	for rows.Next() {
		p := Pair{Person: person, App: app, Kind: kind}
		if err := rows.Scan(&p.AppID, &p.Stamp, &p.Href, &p.ETag); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Postgres) Save(ctx context.Context, p Pair) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO homesync_pairs (person, app, kind, app_id, stamp, href, etag)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (person, app, kind, app_id) DO UPDATE SET stamp = $5, href = $6, etag = $7`,
		p.Person, p.App, string(p.Kind), p.AppID, p.Stamp, p.Href, p.ETag)
	return err
}

func (s *Postgres) Remove(ctx context.Context, p Pair) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM homesync_pairs WHERE person = $1 AND app = $2 AND kind = $3 AND app_id = $4`,
		p.Person, p.App, string(p.Kind), p.AppID)
	return err
}
