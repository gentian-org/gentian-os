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
