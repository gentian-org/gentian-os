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
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/kernel"
)

// The MariaDB scripts against a MariaDB server.
//
// What an app's user can do is decided by the server, not by the text of a
// grant, so this runs the provisioning, dump, restore and destroy scripts
// against the server image the platform's chart pins, with the client image
// the Jobs run. It needs docker and is run when asked for:
//
//	GENTIAN_LIVE_MARIADB=1 go test ./internal/backup -run TestMariaDBAgainstAServer -v

const liveRootPassword = "live-root"

type liveMariaDB struct {
	t      *testing.T
	server string
	work   string
}

// pinnedMariaDBServerImage reads the server image the chart deploys.
func pinnedMariaDBServerImage(t *testing.T) string {
	t.Helper()
	values, err := os.ReadFile(filepath.Join("..", "..", "charts", "infra", "mariadb", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tag := regexp.MustCompile(`(?m)^  tag: "(\d[^"]+)"`).FindSubmatch(values)
	if tag == nil {
		t.Fatal("the mariadb chart's values pin no image tag")
	}
	return "mariadb:" + string(tag[1])
}

func startLiveMariaDB(t *testing.T) *liveMariaDB {
	t.Helper()
	if os.Getenv("GENTIAN_LIVE_MARIADB") == "" {
		t.Skip("set GENTIAN_LIVE_MARIADB=1 to run the scripts against a MariaDB server in docker")
	}
	name := fmt.Sprintf("gentian-mariadb-live-%d", os.Getpid())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "MARIADB_ROOT_PASSWORD="+liveRootPassword, pinnedMariaDBServerImage(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	// The scratch directory is a volume of the daemon's, as a Job's is the
	// kubelet's: nothing here depends on how the daemon maps users.
	m := &liveMariaDB{t: t, server: name, work: name + "-work"}
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", m.work).Run() })
	deadline := time.Now().Add(90 * time.Second)
	for {
		// Over TCP: the image's first, socket-only start does not listen there.
		if _, err := m.try("root", liveRootPassword, "SELECT 1"); err == nil {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatal("the MariaDB server did not come up")
		}
		time.Sleep(time.Second)
	}
}

// script runs one of the Jobs' scripts as its Job would: in the client
// image, as the server's admin, with the scratch directory mounted.
func (m *liveMariaDB) script(shell, script string, env ...string) (string, error) {
	args := []string{"run", "--rm", "-i", "--network", "container:" + m.server,
		"-v", m.work + ":" + workDir,
		"-e", "MYSQL_HOST=127.0.0.1", "-e", "MYSQL_TCP_PORT=3306", "-e", "MYSQL_ADMIN_USER=root", "-e", "MYSQL_PWD=" + liveRootPassword}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, kernel.DefaultMariaDBProvisionerImage, shell, "-c", script)
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

func (m *liveMariaDB) mustScript(shell, script string, env ...string) string {
	m.t.Helper()
	out, err := m.script(shell, script, env...)
	if err != nil {
		m.t.Fatalf("the script failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}
	return out
}

func (m *liveMariaDB) try(user, password, sql string) (string, error) {
	cmd := exec.Command("docker", "exec", "-i", "-e", "MYSQL_PWD="+password, m.server,
		"mariadb", "--protocol=tcp", "-h127.0.0.1", "-u"+user, "-N", "-s", "--raw")
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (m *liveMariaDB) root(sql string) string {
	m.t.Helper()
	out, err := m.try("root", liveRootPassword, sql)
	if err != nil {
		m.t.Fatalf("%s: %v\n%s", sql, err, out)
	}
	return out
}

// allowed and denied say what an app's user can do.
func (m *liveMariaDB) allowed(user, sql string) {
	m.t.Helper()
	if out, err := m.try(user, "pw-"+user, sql); err != nil {
		m.t.Errorf("%s could not: %s\n%s", user, sql, out)
	}
}

func (m *liveMariaDB) denied(user, sql string) {
	m.t.Helper()
	if out, err := m.try(user, "pw-"+user, sql); err == nil {
		m.t.Errorf("%s could: %s\n%s", user, sql, out)
	} else if !strings.Contains(out, "denied") {
		m.t.Errorf("%s: %s failed for another reason than its rights: %s", user, sql, out)
	}
}

func (m *liveMariaDB) setup(db, user string, dynamic bool) (string, error) {
	return m.script("bash", MariaDBSetupScript(db, user, dynamic), "DB_PASS=pw-"+user)
}

func (m *liveMariaDB) grants(user string) string {
	m.t.Helper()
	var kept []string
	for _, line := range strings.Split(m.root(fmt.Sprintf("SHOW GRANTS FOR '%s'@'%%'", user)), "\n") {
		if !strings.HasPrefix(line, "GRANT USAGE ON *.*") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func TestMariaDBAgainstAServer(t *testing.T) {
	m := startLiveMariaDB(t)
	must := func(out string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}

	// Two apps of tenant demo whose names share a prefix without the
	// underscore: crm creates databases of its own, crm2 does not.
	must(m.setup("demo_crm2", "demo_crm2", false))
	must(m.setup("demo_crm", "demo_crm", true))
	// Databases that are nobody's app's, named to sit beside the prefix.
	m.root("CREATE DATABASE demo_crmx; CREATE DATABASE demoXcrm; CREATE DATABASE `demoXcrm_a`; CREATE DATABASE `DEMO_CRM_UP`; CREATE DATABASE elsewhere")

	// --- what the users hold, as the server states it
	if got, want := m.grants("demo_crm"), "GRANT ALL PRIVILEGES ON `demo\\_crm`.* TO `demo_crm`@`%`\n"+
		"GRANT ALL PRIVILEGES ON `demo\\_crm\\_%`.* TO `demo_crm`@`%`"; got != want {
		t.Errorf("demo_crm holds:\n%s\nwant:\n%s", got, want)
	}
	if got, want := m.grants("demo_crm2"), "GRANT ALL PRIVILEGES ON `demo\\_crm2`.* TO `demo_crm2`@`%`"; got != want {
		t.Errorf("demo_crm2 holds:\n%s\nwant:\n%s", got, want)
	}

	// --- inside the prefix, with no global privilege
	m.allowed("demo_crm", "CREATE TABLE demo_crm.t (i INT); INSERT INTO demo_crm.t VALUES (1)")
	m.allowed("demo_crm", "CREATE DATABASE demo_crm_reports; CREATE TABLE demo_crm_reports.r (i INT); INSERT INTO demo_crm_reports.r VALUES (7)")
	m.allowed("demo_crm", "CREATE DATABASE `demo_crm_a-b ``c`; CREATE TABLE `demo_crm_a-b ``c`.r (i INT); INSERT INTO `demo_crm_a-b ``c`.r VALUES (8)")
	m.allowed("demo_crm", "CREATE DATABASE demo_crm_gone; DROP DATABASE demo_crm_gone")
	// --- outside it
	for _, sql := range []string{
		"CREATE DATABASE demo_crmx_new", "CREATE DATABASE other", "CREATE DATABASE demoXcrm_b", "CREATE DATABASE DEMO_CRM_low",
		"SELECT 1 FROM demo_crm2.t", "USE demo_crm2", "USE demo_crmx", "USE demoXcrm", "USE demoXcrm_a", "USE DEMO_CRM_UP", "USE elsewhere",
		"DROP DATABASE demo_crm2", "DROP DATABASE elsewhere",
		"CREATE USER 'made'@'%'", "GRANT SELECT ON demo_crm.* TO 'demo_crm2'@'%'", "SELECT COUNT(*) FROM mysql.global_priv",
		"FLUSH PRIVILEGES", "SELECT 1 INTO OUTFILE '/tmp/out'", "SET GLOBAL max_connections = 1", "SHUTDOWN",
	} {
		m.denied("demo_crm", sql)
	}
	if seen := m.asUser("demo_crm", "SHOW DATABASES"); strings.Contains(seen, "demo_crm2") || strings.Contains(seen, "elsewhere") {
		t.Errorf("demo_crm sees other databases:\n%s", seen)
	}
	// An app that does not create its own has its database and no other.
	m.allowed("demo_crm2", "CREATE TABLE demo_crm2.t (i INT)")
	for _, sql := range []string{"CREATE DATABASE demo_crm2_more", "USE demo_crm", "USE demo_crm_reports", "USE demoXcrm2"} {
		m.denied("demo_crm2", sql)
	}

	// --- a user provisioned as it used to be comes down to the same grants
	m.root("GRANT ALL PRIVILEGES ON *.* TO 'demo_crm'@'%' WITH GRANT OPTION; GRANT ALL PRIVILEGES ON demo_crm2.* TO 'demo_crm2'@'%'; GRANT SELECT ON elsewhere.* TO 'demo_crm2'@'%'")
	m.allowed("demo_crm", "USE elsewhere")
	must(m.setup("demo_crm", "demo_crm", true))
	must(m.setup("demo_crm2", "demo_crm2", false))
	m.denied("demo_crm", "USE elsewhere")
	m.denied("demo_crm", "CREATE USER 'made'@'%'")
	if got := m.grants("demo_crm"); strings.Contains(got, "*.*") || strings.Count(got, "\n") != 1 {
		t.Errorf("demo_crm still holds:\n%s", got)
	}
	if got, want := m.grants("demo_crm2"), "GRANT ALL PRIVILEGES ON `demo\\_crm2`.* TO `demo_crm2`@`%`"; got != want {
		t.Errorf("demo_crm2 holds:\n%s\nwant:\n%s", got, want)
	}
	// And an app that no longer creates its own loses the prefix.
	must(m.setup("demo_crm", "demo_crm", false))
	m.denied("demo_crm", "CREATE DATABASE demo_crm_later")
	must(m.setup("demo_crm", "demo_crm", true))
	// A password is never part of a statement.
	if out, err := m.script("bash", MariaDBSetupScript("demo_odd", "demo_odd", false), `DB_PASS=a'b"c\d$e`); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := m.try("demo_odd", `a'b"c\d$e`, "SELECT 1"); err != nil {
		t.Errorf("the user cannot sign in with the password it was given: %s", out)
	}

	// --- names that overlap are refused, and nothing is made
	// crm-extra in tenant demo, or any app of a tenant demo-crm, would be
	// provisioned demo_crm_extra: inside what demo_crm may touch.
	if out, err := m.setup("demo_crm_extra", "demo_crm_extra", false); err == nil || !strings.Contains(out, "refused") {
		t.Errorf("a database under another app's prefix was provisioned: %v\n%s", err, out)
	}
	if got := m.root("SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = 'demo_crm_extra'") +
		m.root("SELECT COUNT(*) FROM mysql.global_priv WHERE User = 'demo_crm_extra'"); got != "00" {
		t.Errorf("the refused provisioning left a database or a user (%s)", got)
	}
	// The other order: the longer name is there first, the shorter one then
	// asks to create its own.
	must(m.setup("shop_x_y", "shop_x_y", false))
	if out, err := m.setup("shop_x", "shop_x", true); err == nil || !strings.Contains(out, "refused") {
		t.Errorf("an app was given a prefix that holds another app's database: %v\n%s", err, out)
	}
	// Without the prefix the two stand side by side...
	must(m.setup("shop_x", "shop_x", false))
	m.denied("shop_x", "USE shop_x_y")

	// --- the one question: which databases are the app's
	owned := func(db, user string) string { return m.root(mariadbOwnedSQL(db, user) + " ORDER BY s.schema_name") }
	if got, want := owned("demo_crm", "demo_crm"), "demo_crm_a-b `c\ndemo_crm_reports"; got != want {
		t.Errorf("demo_crm's databases besides its own:\n%s\nwant:\n%s", got, want)
	}
	if got := owned("demo_crm2", "demo_crm2"); got != "" {
		t.Errorf("demo_crm2 has no other database, got %q", got)
	}
	// ...and the shorter one's purge, export and restore do not reach the longer.
	if got := owned("shop_x", "shop_x"); got != "" {
		t.Errorf("shop_x_y is another app's and was counted as shop_x's: %q", got)
	}

	// --- export: the provisioned database and the archive of the others
	p := JobParams{Namespace: "system-mariadb", Name: "x", Tenant: "demo", App: "crm", Bucket: "b", Prefix: "p"}
	d := Decryption{}
	m.mustScript("sh", containerByName(MariaDBDumpJob(p, "demo_crm"), "mariadb-dump").Args[0])
	m.mustScript("bash", containerByName(MariaDBOwnedDumpJob(p, "demo_crm", "demo_crm"), "mariadb-dump-owned").Args[0])
	if index := m.mustScript("sh", "cat "+workDir+"/owned/INDEX"); index != "demo_crm_a-b `c\ndemo_crm_reports\n" {
		t.Fatalf("INDEX = %q", index)
	}
	// A dump that fails does not pass for one that was taken.
	if out, err := m.script("sh", containerByName(MariaDBDumpJob(p, "not_there"), "mariadb-dump").Args[0]); err == nil {
		t.Errorf("the dump of a database that is not there succeeded:\n%s", out)
	}
	m.mustScript("sh", "rm -f "+workDir+"/dump.sql")

	// --- restore, into another tenant: the same app, named for that tenant
	must(m.setup("other_crm", "other_crm", true))
	m.allowed("other_crm", "CREATE DATABASE other_crm_reports; CREATE TABLE other_crm_reports.stale (i INT)")
	m.allowed("other_crm", "CREATE DATABASE other_crm_kept")
	m.mustScript("sh", containerByName(MariaDBRestoreJob(p, d, MariaDBArtefact("demo_crm"), "other_crm"), "mariadb-restore").Args[0])
	out := m.mustScript("bash", containerByName(
		MariaDBOwnedRestoreJob(p, d, MariaDBOwnedArtefact("demo_crm"), "demo_crm", "other_crm", "other_crm"), "mariadb-restore-owned").Args[0])
	if !strings.Contains(out, "NOTE: other_crm_kept is other_crm's and the bundle does not hold it") {
		t.Errorf("a database the bundle does not hold was not named:\n%s", out)
	}
	if got := m.asUser("other_crm", "SELECT i FROM other_crm.t; SELECT i FROM other_crm_reports.r; SELECT i FROM `other_crm_a-b ``c`.r"); got != "1\n7\n8" {
		t.Errorf("the restored databases hold %q, read as the app", got)
	}
	if got := m.root("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'other_crm_reports' AND table_name = 'stale'"); got != "0" {
		t.Error("a restored database was loaded into, not replaced")
	}
	// A restore does not write over a database that is another account's.
	m.root("CREATE DATABASE held_crm; CREATE DATABASE held_crm_reports; CREATE USER 'held_crm_reports'@'%'; GRANT ALL PRIVILEGES ON `held\\_crm\\_reports`.* TO 'held_crm_reports'@'%'")
	m.mustScript("bash", containerByName(MariaDBOwnedDumpJob(p, "demo_crm", "demo_crm"), "mariadb-dump-owned").Args[0])
	if out, err := m.script("bash", containerByName(
		MariaDBOwnedRestoreJob(p, d, MariaDBOwnedArtefact("demo_crm"), "demo_crm", "held_crm", "held_crm"), "mariadb-restore-owned").Args[0]); err == nil || !strings.Contains(out, "refused") {
		t.Errorf("a restore wrote over another account's database: %v\n%s", err, out)
	}

	// --- purge: every database that is the app's, and nothing beside them
	out = m.mustScript("bash", mariadbDestroyScript("demo_crm", "demo_crm"))
	left := m.root("SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE 'demo%' ORDER BY BINARY schema_name")
	if want := "DEMO_CRM_UP\ndemoXcrm\ndemoXcrm_a\ndemo_crm2\ndemo_crmx\ndemo_odd"; left != want {
		t.Errorf("after the purge of demo_crm:\n%s\nwant:\n%s\n%s", left, want, out)
	}
	if got := m.root("SELECT COUNT(*) FROM mysql.global_priv WHERE User = 'demo_crm'"); got != "0" {
		t.Error("the purge left the user")
	}
	// Run again it finds nothing and says so by succeeding.
	m.mustScript("bash", mariadbDestroyScript("demo_crm", "demo_crm"))
	// The purge of the shorter name leaves the longer one's database, named.
	out = m.mustScript("bash", mariadbDestroyScript("shop_x", "shop_x"))
	if !strings.Contains(out, "NOTE: shop_x_y is named like a database of shop_x") ||
		m.root("SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name IN ('shop_x', 'shop_x_y')") != "1" {
		t.Errorf("the purge of shop_x did not leave shop_x_y alone:\n%s", out)
	}
}

// asUser runs SQL as an app's user and returns what it printed.
func (m *liveMariaDB) asUser(user, sql string) string {
	m.t.Helper()
	out, err := m.try(user, "pw-"+user, sql)
	if err != nil {
		m.t.Errorf("%s: %s: %s", user, sql, out)
	}
	return out
}
