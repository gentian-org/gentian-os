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
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// postgres starts a real server for one test. A statement Go sees as a
// string is only wrong to a server, so the journal's SQL is tried on one.
// Skipped where there is no container runtime.
func postgres(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker; skipping the SQL test")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not running; skipping the SQL test")
	}
	name := fmt.Sprintf("gentian-chaperone-test-%d", time.Now().UnixNano())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=test", "-e", "POSTGRES_DB=chaperone",
		"-P", "postgres:16-alpine").CombinedOutput()
	if err != nil {
		t.Skipf("could not start postgres: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	port, err := exec.Command("docker", "port", name, "5432/tcp").Output()
	if err != nil {
		t.Skipf("could not read the mapped port: %v", err)
	}
	hostPort := strings.TrimSpace(strings.Split(string(port), "\n")[0])
	if i := strings.LastIndex(hostPort, ":"); i >= 0 {
		hostPort = hostPort[i+1:]
	}
	// Until a real query succeeds: the image restarts its server once
	// after initdb, and pg_isready answers for the first one.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("docker", "exec", name, "psql", "-U", "postgres", "-d", "chaperone", "-c", "SELECT 1").Run() == nil {
			return fmt.Sprintf("postgres://postgres:test@127.0.0.1:%s/chaperone?sslmode=disable", hostPort)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Skip("postgres did not become ready")
	return ""
}

func TestTheJournalKeepsALineInItsOwnDatabase(t *testing.T) {
	ctx := context.Background()
	dsn := postgres(t)
	journal, err := OpenJournal(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	// Opening it again finds the table there and leaves it.
	again, err := OpenJournal(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()

	allowed := Line{
		At: 1_900_000_000_000, Tenant: "acme", Person: "anna", Engine: "claude-code", Tool: "pa/list-tasks@1",
		Class: ClassRead, Decision: "allowed", Outcome: "ok", ArgumentsDigest: strings.Repeat("a", 64),
	}
	refused := Line{
		At: 2, Tenant: "acme", Person: "anna", Engine: "flow", Run: "run_1", Mandate: "man_1", Fingerprint: "f1",
		Tool: "pa/gone@1", Decision: "refused", Reason: "no installed adapter offers this tool",
		ArgumentsDigest: strings.Repeat("b", 64),
	}
	for _, l := range []Line{allowed, refused} {
		if err := journal.Add(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := journal.pool.Query(ctx, `SELECT at, person, engine, tool, cls, stage, decision, reason, outcome FROM journal ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var at int64
		var person, engine, tool, decision, reason string
		var class, stage, outcome *string
		if err := rows.Scan(&at, &person, &engine, &tool, &class, &stage, &decision, &reason, &outcome); err != nil {
			t.Fatal(err)
		}
		show := func(s *string) string {
			if s == nil {
				return "-"
			}
			return *s
		}
		got = append(got, fmt.Sprintln(at, person, engine, tool, show(class), show(stage), decision, reason, show(outcome)))
	}
	want := []string{
		"1900000000000 anna claude-code pa/list-tasks@1 read - allowed  ok\n",
		"2 anna flow pa/gone@1 - - refused no installed adapter offers this tool -\n",
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("journal\n%s\nwant\n%s", strings.Join(got, ""), strings.Join(want, ""))
	}
}
