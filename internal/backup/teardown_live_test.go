/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel"
)

// The PostgreSQL destroy script against a server, for a role and databases
// whose names hold the characters that end a quoted identifier and a literal.
// It drops the app's database, the database its role owns beside it and the
// role, leaves a neighbor's alone, and a second run finds nothing to do.
// Run when asked for:
//
//	GENTIAN_LIVE_POSTGRES=1 go test ./internal/backup -run TestTheDestroyScriptAgainstAServer -v
func TestTheDestroyScriptAgainstAServer(t *testing.T) {
	p := startLivePostgres(t)
	ident := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	role, db, owned := `demo_wi"ki'`, `demo_wi'ki "db`, `demo_wi"ki' own'ed`
	p.admin("postgres", fmt.Sprintf(`CREATE ROLE %s LOGIN CREATEDB PASSWORD 'app-pw'`, ident(role)))
	for _, name := range []string{db, owned} {
		p.admin("postgres", fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, ident(name), ident(role)))
	}
	p.admin("postgres", `CREATE DATABASE neighbor`)

	script := postgresDestroyScript(db, role)
	for run := 1; run <= 2; run++ {
		out, err := exec.Command("docker", "run", "--rm", "-i", "--network", "container:"+p.server,
			"-e", "PGHOST=127.0.0.1", "-e", "PGPORT=5432", "-e", "PGUSER=postgres", "-e", "PGPASSWORD="+livePostgresPassword,
			kernel.DefaultPostgresProvisionerImage, "bash", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("run %d: %v\n%s", run, err, out)
		}
	}
	if got := p.admin("postgres", `SELECT string_agg(datname, ',' ORDER BY datname) FROM pg_database WHERE NOT datistemplate`); got != "neighbor,postgres" {
		t.Errorf("databases left: %s, want the neighbor's and the server's own", got)
	}
	if got := p.admin("postgres", fmt.Sprintf(`SELECT count(*) FROM pg_roles WHERE rolname = '%s'`, strings.ReplaceAll(role, `'`, `''`))); got != "0" {
		t.Errorf("the role is still there")
	}
}
