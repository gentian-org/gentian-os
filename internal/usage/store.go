/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package usage records what a tenant's ceiling was and what it consumed
// beneath it, so that "current use" has a history behind it and a month
// resolves to something invoiceable.
//
// The history lives in each tenant's own {tenant}_shell database, beside the
// portal's audit events and notifications, rather than in a platform-wide
// table. A tenant's consumption is tenant data: it goes where the rest of that
// tenant's data goes, it leaves with the tenant, and it is captured by the same
// TenantExport that captures the shell database already. The cost is that a
// cluster-wide roll-up opens one connection per tenant, which is the honest
// price of the isolation and is paid by a screen nobody loads in a loop.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sample is one observation of a tenant's ceiling and what sits under it.
type Sample struct {
	// ObservedAt is when the sample was taken, UTC.
	ObservedAt time.Time `json:"observedAt"`
	// Plan is the ResourcePlan in force, empty when the tenant's quotas match
	// none. Stored per sample rather than only on change, so a query over an
	// interval never has to look outside it to know what was being paid for.
	Plan string `json:"plan,omitempty"`
	// ProductSku is the plan's SKU at the time of sampling. Denormalised
	// deliberately: a plan's SKU can be re-pointed, and an invoice for March
	// must not change because of an edit made in June.
	ProductSku string `json:"productSku,omitempty"`
	// Hard is the enforced ceiling per ResourceQuota key.
	Hard map[string]string `json:"hard"`
	// Used is the committed consumption per ResourceQuota key — what the
	// cluster counts against the ceiling, which is what the tenant is
	// committed to and therefore what is billed.
	Used map[string]string `json:"used"`
	// Actual is live consumption from metrics.k8s.io, in the same units as
	// Used, or nil when metrics-server is absent. Advisory only: it says
	// whether a tenant is right-sized, never what they owe.
	Actual map[string]string `json:"actual,omitempty"`
}

// PlanEvent records a change of plan.
//
// The samples alone would imply the change, but only to the resolution of the
// sampling interval and only while the samples are retained. A plan change is
// the billable event; it is recorded exactly, once, and never trimmed.
type PlanEvent struct {
	OccurredAt time.Time `json:"occurredAt"`
	FromPlan   string    `json:"fromPlan,omitempty"`
	ToPlan     string    `json:"toPlan"`
	ProductSku string    `json:"productSku,omitempty"`
	Actor      string    `json:"actor,omitempty"`
}

const schemaDDL = `
CREATE TABLE IF NOT EXISTS tenant_resource_samples (
    observed_at  TIMESTAMPTZ NOT NULL,
    plan         TEXT        NOT NULL DEFAULT '',
    product_sku  TEXT        NOT NULL DEFAULT '',
    hard         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    used         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    actual       JSONB,
    PRIMARY KEY (observed_at)
);
CREATE INDEX IF NOT EXISTS ix_tenant_resource_samples_observed_at
    ON tenant_resource_samples (observed_at DESC);

CREATE TABLE IF NOT EXISTS tenant_resource_plan_events (
    occurred_at  TIMESTAMPTZ NOT NULL,
    from_plan    TEXT        NOT NULL DEFAULT '',
    to_plan      TEXT        NOT NULL,
    product_sku  TEXT        NOT NULL DEFAULT '',
    actor        TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (occurred_at)
);
CREATE INDEX IF NOT EXISTS ix_tenant_resource_plan_events_occurred_at
    ON tenant_resource_plan_events (occurred_at DESC);
`

// Store reads and writes one tenant's usage history.
//
// It holds no pooled connection. Sampling runs on a slow ticker and the read
// paths serve a console tab, so a connection per call costs less than keeping
// one open per tenant for the lifetime of the operator.
type Store struct {
	dsn string
}

// NewStore returns a store for a tenant database URL.
func NewStore(dsn string) *Store { return &Store{dsn: dsn} }

func (s *Store) connect(ctx context.Context) (*pgx.Conn, error) {
	// The Secret carries a SQLAlchemy-shaped URL because the portal is what
	// normally reads it; pgx does not know the +psycopg dialect suffix.
	dsn := normalizeDSN(s.dsn)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to tenant database: %w", err)
	}
	return conn, nil
}

// EnsureSchema creates the usage tables when they are absent.
//
// Called before each write rather than once at start-up: a tenant provisioned
// after the operator started has no tables, and the alternative is a sampler
// that silently skips every tenant created since the last restart.
func (s *Store) EnsureSchema(ctx context.Context) error {
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, schemaDDL); err != nil {
		return fmt.Errorf("ensure usage schema: %w", err)
	}
	return nil
}

// RecordSample appends one observation.
func (s *Store) RecordSample(ctx context.Context, sample Sample) error {
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	hard, _ := json.Marshal(sample.Hard)
	used, _ := json.Marshal(sample.Used)
	var actual any
	if sample.Actual != nil {
		b, _ := json.Marshal(sample.Actual)
		actual = string(b)
	}

	// ON CONFLICT DO NOTHING, not DO UPDATE: two samples landing on the same
	// instant means the sampler ran twice for one tick, and the first reading
	// is as good as the second. Overwriting would make a retry look like a
	// change in consumption.
	_, err = conn.Exec(ctx, `
        INSERT INTO tenant_resource_samples (observed_at, plan, product_sku, hard, used, actual)
        VALUES ($1, $2, $3, $4, $5, $6)
        ON CONFLICT (observed_at) DO NOTHING`,
		sample.ObservedAt.UTC(), sample.Plan, sample.ProductSku, string(hard), string(used), actual)
	if err != nil {
		return fmt.Errorf("record usage sample: %w", err)
	}
	return nil
}

// RecordPlanEvent appends a plan change.
func (s *Store) RecordPlanEvent(ctx context.Context, event PlanEvent) error {
	conn, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	_, err = conn.Exec(ctx, `
        INSERT INTO tenant_resource_plan_events (occurred_at, from_plan, to_plan, product_sku, actor)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (occurred_at) DO NOTHING`,
		event.OccurredAt.UTC(), event.FromPlan, event.ToPlan, event.ProductSku, event.Actor)
	if err != nil {
		return fmt.Errorf("record plan event: %w", err)
	}
	return nil
}

// Samples returns observations in [from, to), oldest first.
//
// step thins the result to at most one row per interval, chosen as the newest
// in each bucket. A year of quarter-hourly samples is 35,000 rows and no chart
// benefits from receiving them; thinning in SQL keeps that decision out of the
// API and the browser both.
func (s *Store) Samples(ctx context.Context, from, to time.Time, step time.Duration) ([]Sample, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()

	if step <= 0 {
		step = time.Minute
	}
	rows, err := conn.Query(ctx, `
        SELECT DISTINCT ON (bucket) observed_at, plan, product_sku, hard, used, actual
        FROM (
            SELECT *, to_timestamp(floor(extract(epoch FROM observed_at) / $3) * $3) AS bucket
            FROM tenant_resource_samples
            WHERE observed_at >= $1 AND observed_at < $2
        ) t
        ORDER BY bucket, observed_at DESC`,
		from.UTC(), to.UTC(), step.Seconds())
	if err != nil {
		return nil, fmt.Errorf("query usage samples: %w", err)
	}
	defer rows.Close()

	var out []Sample
	for rows.Next() {
		var (
			sample     Sample
			hard, used []byte
			actual     []byte
		)
		if err := rows.Scan(&sample.ObservedAt, &sample.Plan, &sample.ProductSku, &hard, &used, &actual); err != nil {
			return nil, fmt.Errorf("scan usage sample: %w", err)
		}
		_ = json.Unmarshal(hard, &sample.Hard)
		_ = json.Unmarshal(used, &sample.Used)
		if len(actual) > 0 {
			_ = json.Unmarshal(actual, &sample.Actual)
		}
		out = append(out, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read usage samples: %w", err)
	}
	// DISTINCT ON forces an ORDER BY on the bucket, which is already
	// chronological, so the result arrives oldest first as callers expect.
	return out, nil
}

// PlanEvents returns plan changes in [from, to), oldest first.
func (s *Store) PlanEvents(ctx context.Context, from, to time.Time) ([]PlanEvent, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, `
        SELECT occurred_at, from_plan, to_plan, product_sku, actor
        FROM tenant_resource_plan_events
        WHERE occurred_at >= $1 AND occurred_at < $2
        ORDER BY occurred_at`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("query plan events: %w", err)
	}
	defer rows.Close()

	var out []PlanEvent
	for rows.Next() {
		var e PlanEvent
		if err := rows.Scan(&e.OccurredAt, &e.FromPlan, &e.ToPlan, &e.ProductSku, &e.Actor); err != nil {
			return nil, fmt.Errorf("scan plan event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read plan events: %w", err)
	}
	return out, nil
}

// LastPlanBefore returns the plan in force immediately before t, so an interval
// that contains no plan change still knows what it started on.
func (s *Store) LastPlanBefore(ctx context.Context, t time.Time) (PlanEvent, bool, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return PlanEvent{}, false, err
	}
	defer func() { _ = conn.Close(ctx) }()

	var e PlanEvent
	err = conn.QueryRow(ctx, `
        SELECT occurred_at, from_plan, to_plan, product_sku, actor
        FROM tenant_resource_plan_events
        WHERE occurred_at < $1
        ORDER BY occurred_at DESC
        LIMIT 1`, t.UTC()).Scan(&e.OccurredAt, &e.FromPlan, &e.ToPlan, &e.ProductSku, &e.Actor)
	if err != nil {
		if err == pgx.ErrNoRows {
			return PlanEvent{}, false, nil
		}
		return PlanEvent{}, false, fmt.Errorf("query last plan event: %w", err)
	}
	return e, true, nil
}

// Prune deletes samples older than the cutoff.
//
// Plan events are never pruned: they are the billing record, they accrue at the
// rate a tenant changes plan rather than at the rate of the clock, and an
// invoice that cannot be reconstructed is worse than a table that grows by a
// handful of rows a year.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	conn, err := s.connect(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(ctx) }()

	tag, err := conn.Exec(ctx,
		`DELETE FROM tenant_resource_samples WHERE observed_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("prune usage samples: %w", err)
	}
	return tag.RowsAffected(), nil
}
