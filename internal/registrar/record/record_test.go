/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package record_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gentian-org/gentian-os/internal/registrar/record"
)

// A real Postgres, because the thing under test is SQL.
//
// The schema in this package compiled perfectly well while it said
// "BIGGENERATED" where it meant "BIGINT GENERATED ALWAYS AS IDENTITY": Go sees
// a string, and only a server rejects it. A test that faked the database would
// have passed on that too, which is the whole argument for this one.
//
// Skipped where there is no container runtime, so a machine without Docker
// still runs the rest of the suite.
func postgres(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker; skipping the SQL test")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not running; skipping the SQL test")
	}
	name := fmt.Sprintf("gentian-record-test-%d", time.Now().UnixNano())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=test", "-e", "POSTGRES_DB=registrar",
		"-P", "postgres:16-alpine").CombinedOutput()
	if err != nil {
		t.Skipf("could not start postgres: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	port, err := exec.Command("docker", "port", name, "5432/tcp").Output()
	if err != nil {
		t.Skipf("could not read the mapped port: %v", err)
	}
	hostPort := strings.TrimSpace(string(port))
	if i := strings.LastIndex(hostPort, ":"); i >= 0 {
		hostPort = hostPort[i+1:]
	}
	dsn := fmt.Sprintf("postgres://postgres:test@127.0.0.1:%s/registrar?sslmode=disable", hostPort)

	// Wait until a real query succeeds, not until pg_isready says yes.
	//
	// The official image runs initdb against a temporary server and then
	// RESTARTS it, and pg_isready answers for the first one. A test that
	// trusted it connected into the shutdown and got "unexpected EOF" —
	// intermittently, depending on which side of the restart it landed.
	// Asking the database the question the code will ask is the only
	// readiness check that cannot race with that.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		err := exec.Command("docker", "exec", name,
			"psql", "-U", "postgres", "-d", "registrar", "-c", "SELECT 1").Run()
		if err == nil {
			return dsn
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Skip("postgres did not become ready")
	return ""
}

func TestTheRecordSurvivesARoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := record.Open(ctx, postgres(t), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(store.Close)

	want := record.Action{
		RequestID: "req-abc123", Action: "invite", Realm: "tenant-demo",
		Tenant: "demo", Target: "tom@example.com", Principal: "u-alice",
		Decision: "can_manage_users tenant:demo",
	}
	if err := store.Write(ctx, want); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := store.ByTenant(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read back %d rows, want 1", len(got))
	}
	g := got[0]
	if g.RequestID != want.RequestID || g.Action != want.Action || g.Target != want.Target ||
		g.Principal != want.Principal || g.Decision != want.Decision {
		t.Fatalf("round trip = %+v", g)
	}
	// The time is the server's when the caller gave none, and it is a real
	// time rather than a zero somebody would have to interpret.
	if g.At.IsZero() || time.Since(g.At) > time.Hour {
		t.Fatalf("occurred_at = %v", g.At)
	}
	// Another tenant's record is not this tenant's.
	if other, err := store.ByTenant(ctx, "solo", 10); err != nil || len(other) != 0 {
		t.Fatalf("tenant solo saw %d rows (%v)", len(other), err)
	}
}

// Opening twice must be safe: the registrar restarts, and a schema that could
// only be created once would make the second start fail.
func TestOpeningTwiceIsSafe(t *testing.T) {
	ctx := context.Background()
	dsn := postgres(t)
	first, err := record.Open(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	first.Close()
	second, err := record.Open(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	second.Close()
}

// Personal data does not keep itself. A row past the horizon goes, and one
// inside it stays.
func TestPruneRemovesWhatIsPastTheHorizon(t *testing.T) {
	ctx := context.Background()
	store, err := record.Open(ctx, postgres(t), time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(store.Close)

	old := record.Action{
		RequestID: "old", Action: "invite", Realm: "r", Tenant: "demo",
		Target: "t", Principal: "p", Decision: "d",
		At: time.Now().UTC().Add(-48 * time.Hour),
	}
	recent := old
	recent.RequestID = "recent"
	recent.At = time.Now().UTC()
	for _, a := range []record.Action{old, recent} {
		if err := store.Write(ctx, a); err != nil {
			t.Fatalf("write %s: %v", a.RequestID, err)
		}
	}

	n, err := store.Prune(ctx)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want the one past the horizon", n)
	}
	got, err := store.ByTenant(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].RequestID != "recent" {
		t.Fatalf("after pruning: %+v", got)
	}
}

// A store that was never opened is not an error at every call site. The
// registrar runs without one on a cluster that has not provisioned the
// database, and an invite must not fail because the record cannot be kept.
func TestANilStoreIsSilent(t *testing.T) {
	var store *record.Store
	if err := store.Write(context.Background(), record.Action{}); err != nil {
		t.Fatalf("write on a nil store: %v", err)
	}
	if n, err := store.Prune(context.Background()); err != nil || n != 0 {
		t.Fatalf("prune on a nil store: %d, %v", n, err)
	}
	store.Close()
}

// An administrator's invitation waits here for its tenant: one request per
// tenant, replaced by a second, held by whoever attempts it, and gone with
// its outcome recorded in its place.
func TestAnInvitationIsKeptReplacedAndSettled(t *testing.T) {
	ctx := context.Background()
	store, err := record.Open(ctx, postgres(t), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(store.Close)
	asked := time.Now().UTC().Truncate(time.Second)
	request := func(address, requestID string) record.Invitation {
		return record.Invitation{
			Tenant: "newco", Email: address, Principal: "u-alice", Decision: "can_configure cluster:c1",
			RequestID: requestID, RequestedAt: asked, ExpiresAt: asked.Add(24 * time.Hour),
		}
	}

	if st, err := store.InvitationState(ctx, "newco"); err != nil || st.State != record.InvitationNone {
		t.Fatalf("before anything was asked: %+v (%v)", st, err)
	}
	if err := store.RequestInvitation(ctx, request("owner@example.org", "req-first1")); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := store.RequestInvitation(ctx, request("second@example.org", "req-second")); err != nil {
		t.Fatalf("second request: %v", err)
	}
	waiting, err := store.PendingInvitations(ctx)
	if err != nil || len(waiting) != 1 {
		t.Fatalf("one request per tenant, got %+v (%v)", waiting, err)
	}
	if w := waiting[0]; w.Email != "second@example.org" || w.RequestID != "req-second" || w.Principal != "u-alice" ||
		!w.RequestedAt.Equal(asked) || !w.ExpiresAt.Equal(asked.Add(24*time.Hour)) {
		t.Fatalf("kept as %+v", w)
	}
	if st, err := store.InvitationState(ctx, "newco"); err != nil || st.State != record.InvitationPending ||
		!st.ExpiresAt.Equal(asked.Add(24*time.Hour)) {
		t.Fatalf("while it waits: %+v (%v)", st, err)
	}

	// No outcome leaves it waiting.
	if err := store.AttemptInvitation(ctx, "newco", func(record.Invitation) *record.Action { return nil }); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if waiting, _ := store.PendingInvitations(ctx); len(waiting) != 1 {
		t.Fatalf("an attempt without an outcome removed the request: %+v", waiting)
	}

	// While one attempt holds it, another does not make the same attempt.
	held, release, second := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	first := make(chan error, 1)
	go func() {
		first <- store.AttemptInvitation(ctx, "newco", func(inv record.Invitation) *record.Action {
			close(held)
			<-release
			return &record.Action{
				RequestID: inv.RequestID, Action: record.ActionInvitationNotMailed, Realm: "newco", Tenant: inv.Tenant,
				Target: "admin@newco.example.org", Principal: inv.Principal, Decision: inv.Decision,
				Detail: "HTTP 500: no relay",
			}
		})
	}()
	<-held
	go func() {
		second <- store.AttemptInvitation(ctx, "newco", func(record.Invitation) *record.Action {
			t.Error("a request that is held was attempted a second time")
			return nil
		})
	}()
	if err := <-second; err != nil {
		t.Fatalf("the second attempt: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the first attempt: %v", err)
	}

	if waiting, _ := store.PendingInvitations(ctx); len(waiting) != 0 {
		t.Fatalf("the request outlived its outcome: %+v", waiting)
	}
	st, err := store.InvitationState(ctx, "newco")
	if err != nil || st.State != record.InvitationNotMailed || st.Reason != "HTTP 500: no relay" || st.At.IsZero() {
		t.Fatalf("after it could not be mailed: %+v (%v)", st, err)
	}
	// The record says what was asked and what became of it, and names the
	// account. The address is in neither row.
	actions, err := store.ByTenant(ctx, "newco", 10)
	if err != nil || len(actions) != 3 {
		t.Fatalf("two requests and an outcome, got %+v (%v)", actions, err)
	}
	for _, a := range actions {
		if strings.Contains(fmt.Sprintf("%+v", a), "@example.org") && a.Target != "admin@newco.example.org" {
			t.Errorf("the record carries an address: %+v", a)
		}
		if strings.Contains(fmt.Sprintf("%+v", a), "owner@") || strings.Contains(fmt.Sprintf("%+v", a), "second@") {
			t.Errorf("the record carries the recovery address: %+v", a)
		}
	}

	// An activation by hand afterwards supersedes it.
	if err := store.Write(ctx, record.Action{RequestID: "req-manual", Action: record.ActionActivateAdmin,
		Realm: "newco", Tenant: "newco", Target: "admin@newco.example.org", Principal: "u-alice",
		Decision: "can_configure cluster:c1", At: time.Now().UTC().Add(time.Minute)}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if st, err := store.InvitationState(ctx, "newco"); err != nil || st.State != record.InvitationNone {
		t.Fatalf("after an activation by hand: %+v (%v)", st, err)
	}
}

// A registrar from before the invitations starts on its old database: the
// table it already has gains what it lacks, and the new one is made.
func TestADatabaseFromBeforeTheInvitationsIsBroughtUpToDate(t *testing.T) {
	ctx := context.Background()
	dsn := postgres(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE identity_actions (
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
		INSERT INTO identity_actions (request_id, action, realm, tenant, target, principal, decision)
		VALUES ('req-old00001', 'invite', 'demo', 'demo', 'tom@example.com', 'u-alice', 'can_manage_users tenant:demo');`); err != nil {
		t.Fatalf("the old schema: %v", err)
	}

	store, err := record.Open(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("open on the old database: %v", err)
	}
	t.Cleanup(store.Close)
	old, err := store.ByTenant(ctx, "demo", 10)
	if err != nil || len(old) != 1 || old[0].RequestID != "req-old00001" || old[0].Detail != "" {
		t.Fatalf("what was recorded before: %+v (%v)", old, err)
	}
	if err := store.RequestInvitation(ctx, record.Invitation{Tenant: "newco", Email: "owner@example.org",
		Principal: "u-alice", Decision: "can_configure cluster:c1", RequestID: "req-new00001",
		RequestedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("a request on the old database: %v", err)
	}
}
