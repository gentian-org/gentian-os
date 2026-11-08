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
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/kernel"
)

// The desktop-database scripts against a PostgreSQL server.
//
// What the owner of a database may do there, and what it may not do to the
// database beside it, is decided by the server. This runs the dump, restore
// and destroy scripts of the kernel's desktop database as the Jobs run
// them -- as the database's own role, with no administrator's credential --
// against a server laid out as the kernel's is: the desktop's database, and
// beside it one that is another role's. It needs docker and is run when
// asked for:
//
//	GENTIAN_LIVE_POSTGRES=1 go test ./internal/backup -run TestTheDesktopDatabaseAgainstAServer -v

type livePostgres struct {
	t      *testing.T
	server string
	work   string
}

const livePostgresPassword = "live-admin"

func startLivePostgres(t *testing.T) *livePostgres {
	t.Helper()
	if os.Getenv("GENTIAN_LIVE_POSTGRES") == "" {
		t.Skip("set GENTIAN_LIVE_POSTGRES=1 to run the scripts against a PostgreSQL server in docker")
	}
	name := fmt.Sprintf("gentian-postgres-live-%d", os.Getpid())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD="+livePostgresPassword, kernel.DefaultPostgresProvisionerImage).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	p := &livePostgres{t: t, server: name, work: name + "-work"}
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", p.work).Run() })
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := p.sql("postgres", livePostgresPassword, "postgres", "SELECT 1"); err == nil {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatal("the PostgreSQL server did not come up")
		}
		time.Sleep(time.Second)
	}
}

func (p *livePostgres) sql(user, password, database, sql string) (string, error) {
	cmd := exec.Command("docker", "run", "--rm", "-i", "--network", "container:"+p.server,
		"-e", "PGPASSWORD="+password, kernel.DefaultPostgresProvisionerImage,
		"psql", "-h", "127.0.0.1", "-U", user, "-d", database, "-v", "ON_ERROR_STOP=1", "-tA")
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (p *livePostgres) admin(database, sql string) string {
	p.t.Helper()
	out, err := p.sql("postgres", livePostgresPassword, database, sql)
	if err != nil {
		p.t.Fatalf("%s: %v\n%s", sql, err, out)
	}
	return out
}

// asOwner runs one of the Jobs' scripts as its Job would: in the client
// image, as the desktop database's role, with the scratch directory.
func (p *livePostgres) asOwner(script string, env ...string) (string, error) {
	args := []string{"run", "--rm", "-i", "--network", "container:" + p.server, "-v", p.work + ":" + workDir,
		"-e", "PGHOST=127.0.0.1", "-e", "PGPORT=5432", "-e", "PGDATABASE=" + KernelDesktopDatabase,
		"-e", "PGUSER=" + KernelDesktopRole, "-e", "PGPASSWORD=owner-pw"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, kernel.DefaultPostgresProvisionerImage, "sh", "-c", script)
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

func TestTheDesktopDatabaseAgainstAServer(t *testing.T) {
	p := startLivePostgres(t)
	params, d := mailParams()

	// The server as the kernel's chart lays it out: the desktop's database
	// owned by its role, and beside it one that is another role's.
	p.admin("postgres", fmt.Sprintf(`CREATE ROLE %[2]s LOGIN PASSWORD 'owner-pw';
CREATE ROLE keycloak_user LOGIN PASSWORD 'kc-pw';`, KernelDesktopDatabase, KernelDesktopRole))
	p.admin("postgres", fmt.Sprintf("CREATE DATABASE %s OWNER %s", KernelDesktopDatabase, KernelDesktopRole))
	p.admin("postgres", "CREATE DATABASE keycloak OWNER keycloak_user")
	if _, err := p.sql("keycloak_user", "kc-pw", "keycloak", "CREATE TABLE realm (name text); INSERT INTO realm VALUES ('kernel')"); err != nil {
		t.Fatal(err)
	}
	fill := `CREATE TABLE user_shell_prefs (user_sub text, tenant text, prefs_json jsonb, PRIMARY KEY (user_sub, tenant));
INSERT INTO user_shell_prefs VALUES ('sub-1', 'platform', '{"dock": ["mail", "files"]}'), ('sub-2', 'platform', '{"theme": "dark"}');
CREATE TABLE admin_notifications (id text PRIMARY KEY, tenant text, title text);
INSERT INTO admin_notifications VALUES ('n1', 'platform', 'maintenance on Friday');
CREATE SEQUENCE visits; SELECT nextval('visits'); SELECT nextval('visits');`
	if out, err := p.sql(KernelDesktopRole, "owner-pw", KernelDesktopDatabase, fill); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	state := func() string {
		t.Helper()
		return p.admin(KernelDesktopDatabase, `SELECT 'prefs', user_sub, tenant, prefs_json::text FROM user_shell_prefs ORDER BY 2;
SELECT 'notice', id, title FROM admin_notifications ORDER BY 2;
SELECT 'visits', last_value FROM visits;
SELECT 'owner', c.relname, r.rolname FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner
 WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'S') ORDER BY 2;`)
	}
	before := state()
	if !strings.Contains(before, "sub-2|platform") || !strings.Contains(before, "maintenance on Friday") {
		t.Fatalf("the database before the backup:\n%s", before)
	}

	// --- back up: the Job's dump container, as the owner.
	dump := containerByName(KernelDesktopDumpJob(params), "pg-dump").Args[0]
	p.mustOwner(dump)

	// --- what was written since the backup is replaced by a restore.
	if out, err := p.sql(KernelDesktopRole, "owner-pw", KernelDesktopDatabase,
		`UPDATE user_shell_prefs SET prefs_json = '{"theme": "light"}' WHERE user_sub = 'sub-2';
DELETE FROM admin_notifications; INSERT INTO user_shell_prefs VALUES ('sub-3', 'platform', '{}'); SELECT nextval('visits');`); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	restore := containerByName(KernelDesktopRestoreJob(params, d, PostgresArtefact(KernelDesktopDatabase)), "pg-restore").Args[0]
	p.mustOwner(restore)
	if got := state(); got != before {
		t.Errorf("after a restore over a changed database:\n%s\nbefore the backup:\n%s", got, before)
	}

	// --- purge: the destroy Job's script empties it, and it alone.
	destroy := kernelDesktopDestroyScript()
	out := p.mustOwner(destroy)
	if !strings.Contains(out, "is empty") {
		t.Errorf("the destroy script:\n%s", out)
	}
	if left := p.admin(KernelDesktopDatabase, "SELECT count(*) FROM pg_class WHERE relnamespace = 'public'::regnamespace"); left != "0" {
		t.Errorf("%s object(s) are left in the desktop's database", left)
	}
	if got := p.admin("postgres", "SELECT datname FROM pg_database WHERE datname IN ('portal_shell', 'keycloak') ORDER BY 1"); got != "keycloak\nportal_shell" {
		t.Errorf("after the purge the server has the databases:\n%s", got)
	}
	if got := p.admin("keycloak", "SELECT name FROM realm"); got != "kernel" {
		t.Errorf("the database beside the desktop's was changed: %q", got)
	}
	// Again, on an empty database: still success.
	p.mustOwner(destroy)

	// --- restore after the purge: everything is back.
	p.mustOwner(restore)
	if got := state(); got != before {
		t.Errorf("after purge and restore:\n%s\nbefore the backup:\n%s", got, before)
	}

	// --- a credential that opens a database that is not its role's changes
	// nothing there: refused, by both.
	p.admin("postgres", "GRANT CONNECT ON DATABASE keycloak TO "+KernelDesktopRole)
	for name, script := range map[string]string{"restore": restore, "destroy": destroy} {
		out, err := p.asOwner(script, "PGDATABASE=keycloak")
		if err == nil || !strings.Contains(out, "refused") {
			t.Errorf("%s into another role's database was not refused: %v\n%s", name, err, out)
		}
	}
	if got := p.admin("keycloak", "SELECT name FROM realm"); got != "kernel" {
		t.Errorf("a refused Job changed the database beside the desktop's: %q", got)
	}
}

func (p *livePostgres) mustOwner(script string) string {
	p.t.Helper()
	out, err := p.asOwner(script)
	if err != nil {
		p.t.Fatalf("the script failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}
	return out
}
