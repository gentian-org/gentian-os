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

// The restore of an app's database against a PostgreSQL server, for a dump
// taken of another tenant's database: a bundle brought to another tenant.
//
// The app's tables are in a schema of its database's name, which its role
// reads first. A dump of corp's database names that schema corp_...; loaded
// into test's it went in beside test's own, and the app went on reading its
// own. A view in a dependency loop, as Odoo has, failed the load outright.
// Run when asked for:
//
//	GENTIAN_LIVE_POSTGRES=1 go test ./internal/backup -run TestARestoreFromAnotherTenantsDump -v
func TestARestoreFromAnotherTenantsDumpIsTheDatabasesOwnSchema(t *testing.T) {
	p := startLivePostgres(t)
	for _, tenant := range []string{"corp", "test"} {
		role, db := tenant+"_odoo-base-ce", tenant+"_odoo_base_ce"
		// As provisioning makes them (database_reconciler.go).
		p.admin("postgres", fmt.Sprintf(`CREATE ROLE "%[1]s" LOGIN PASSWORD 'app-pw';
CREATE DATABASE %[2]s OWNER "%[1]s";
ALTER ROLE "%[1]s" SET search_path TO "%[2]s", public;`, role, db))
		p.admin(db, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS "%[2]s" AUTHORIZATION "%[1]s";`, role, db))
		fill := fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE TABLE purchase_order_line (id serial PRIMARY KEY, name text, qty numeric);
CREATE INDEX pol_name ON purchase_order_line USING gin (name gin_trgm_ops);
CREATE VIEW vendor_delay_report AS SELECT id, qty FROM purchase_order_line;
CREATE FUNCTION open_qty() RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT count(*) FROM vendor_delay_report; END;
CREATE OR REPLACE VIEW vendor_delay_report AS SELECT id, qty, CASE WHEN false THEN open_qty() END AS n FROM purchase_order_line;
INSERT INTO purchase_order_line (name, qty) VALUES ('%[1]s widget', 2), ('%[1]s gadget', 3);`, tenant)
		if out, err := p.sql(role, "app-pw", db, fill); err != nil {
			t.Fatalf("%s: %v\n%s", tenant, err, out)
		}
	}
	if out, err := p.sql("test_odoo-base-ce", "app-pw", "test_odoo_base_ce", "CREATE TABLE only_in_test (id int)"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// corp's dump, as a backup takes it.
	if out, err := p.inWork("pg_dump --format=custom --no-owner --no-acl --dbname=corp_odoo_base_ce --file=" + workDir + "/dump.pgc"); err != nil {
		t.Fatalf("dump: %v\n%s", err, out)
	}

	restore := func(role, db string) (string, error) {
		job := PostgresRestoreJob(JobParams{Role: role}, recipientDecryption(), PostgresArtefact(db), db)
		return p.inWork(containerByName(job, "pg-restore").Args[0])
	}
	if out, err := restore("test_odoo-base-ce", "test_odoo_base_ce"); err != nil {
		t.Fatalf("restore into test: %v\n%s", err, out)
	}
	if got := p.admin("test_odoo_base_ce", `SELECT string_agg(nspname, ',' ORDER BY nspname) FROM pg_namespace
WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'`); got != "public,test_odoo_base_ce" {
		t.Errorf("schemas after the restore: %s, want test's own only", got)
	}
	// What the app reads through its search_path is corp's, and works.
	got, err := p.sql("test_odoo-base-ce", "app-pw", "test_odoo_base_ce", `SELECT string_agg(name, ',' ORDER BY id) FROM purchase_order_line;
SELECT count(*) FROM vendor_delay_report;
SELECT open_qty();
SELECT similarity('widget', 'wigdet') > 0;
INSERT INTO purchase_order_line (name) VALUES ('new') RETURNING id;
SELECT to_regclass('only_in_test') IS NULL;`)
	if err != nil || got != "corp widget,corp gadget\n2\n2\nt\n3\nINSERT 0 1\nt" {
		t.Errorf("the app reads %q (%v), want corp's rows, its view, function and sequence, and test's own gone", got, err)
	}

	// Again: a retry of a restore that already ran.
	if out, err := restore("test_odoo-base-ce", "test_odoo_base_ce"); err != nil {
		t.Fatalf("restore again: %v\n%s", err, out)
	}
	// Into the tenant the dump is of: loaded as it is.
	if out, err := restore("corp_odoo-base-ce", "corp_odoo_base_ce"); err != nil || !strings.Contains(out, "restored corp_odoo_base_ce\n") {
		t.Fatalf("restore into corp: %v\n%s", err, out)
	}
}

// inWork runs a script as a restore Job's loading container does: with the
// administrator's connection and the scratch directory.
func (p *livePostgres) inWork(script string) (string, error) {
	out, err := exec.Command("docker", "run", "--rm", "-i", "--network", "container:"+p.server, "-v", p.work+":"+workDir,
		"-e", "PGHOST=127.0.0.1", "-e", "PGPORT=5432", "-e", "PGUSER=postgres", "-e", "PGPASSWORD="+livePostgresPassword,
		kernel.DefaultPostgresProvisionerImage, "sh", "-c", script).CombinedOutput()
	return string(out), err
}
