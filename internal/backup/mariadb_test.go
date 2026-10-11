/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel"
)

// The Jobs that dump and load run the client of the server they talk to: a
// dump written by a newer mariadb-dump is one the pinned server refuses to
// load, which nothing shows until a restore.
func TestTheMariaDBClientIsTheServersImage(t *testing.T) {
	server := pinnedMariaDBServerImage(t)
	if kernel.DefaultMariaDBProvisionerImage != server {
		t.Errorf("the Jobs run %s and the chart deploys %s", kernel.DefaultMariaDBProvisionerImage, server)
	}
	values, err := os.ReadFile(filepath.Join("..", "..", "charts", "gentian-os", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values), "\n  mariadb: \""+server+"\"\n") {
		t.Errorf("the operator chart's provisioners.mariadb is not %s", server)
	}
}

// like matches a name against a pattern as MariaDB matches a grant's
// database pattern and LIKE: "_" is any one character, "%" any run, and a
// backslash makes the next character itself.
func like(pattern, name string) bool {
	if pattern == "" {
		return name == ""
	}
	switch pattern[0] {
	case '%':
		for i := 0; i <= len(name); i++ {
			if like(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	case '_':
		return name != "" && like(pattern[1:], name[1:])
	case '\\':
		if len(pattern) > 1 {
			return name != "" && name[0] == pattern[1] && like(pattern[2:], name[1:])
		}
	}
	return name != "" && name[0] == pattern[0] && like(pattern[1:], name[1:])
}

func mariadbScripts(db, user string) map[string]string {
	p := JobParams{Namespace: "system-s3", Name: "j", Tenant: "demo", App: "crm", Bucket: "b", Prefix: "p",
		Encryption: Encryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, Recipients: []string{"age1qqqsyqcyq5rqwzqfpg9scrgwpugpzysnzs23v9ccrydpk8qarc0sxpzkh"}}}
	d := Decryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, SecretName: "k", SecretKey: "identity"}
	return map[string]string{
		"provisioning":          MariaDBSetupScript(db, user, false),
		"provisioning, dynamic": MariaDBSetupScript(db, user, true),
		"purge":                 mariadbDestroyScript(db, user),
		"export":                containerByName(MariaDBOwnedDumpJob(p, user, db), "mariadb-dump-owned").Args[0],
		"restore":               containerByName(MariaDBOwnedRestoreJob(p, d, MariaDBOwnedArtefact("old_crm"), "old_crm", db, user), "mariadb-restore-owned").Args[0],
		"export, provisioned":   containerByName(MariaDBDumpJob(p, db), "mariadb-dump").Args[0],
		"restore, provisioned":  containerByName(MariaDBRestoreJob(p, d, MariaDBArtefact(db), db), "mariadb-restore").Args[0],
	}
}

// What an app's user is granted, to the character: its database, and for an
// app that creates its own, the databases under its prefix -- with the
// underscores escaped, because in a grant an underscore is a wildcard.
func TestMariaDBGrantsAreOnTheAppsDatabasesAndNothingElse(t *testing.T) {
	static := MariaDBGrants("demo_crm", "demo_crm", false)
	if len(static) != 1 || static[0] != "GRANT ALL PRIVILEGES ON `demo\\_crm`.* TO 'demo_crm'@'%'" {
		t.Errorf("an app that does not create databases is granted %q", static)
	}
	dynamic := MariaDBGrants("demo_crm", "demo_crm", true)
	if len(dynamic) != 2 || dynamic[0] != static[0] ||
		dynamic[1] != "GRANT ALL PRIVILEGES ON `demo\\_crm\\_%`.* TO 'demo_crm'@'%'" {
		t.Errorf("an app that creates databases is granted %q", dynamic)
	}
	if got := MariaDBRevokeAll("demo_crm"); got != "REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'demo_crm'@'%'" {
		t.Errorf("revoke = %q", got)
	}

	for _, isDynamic := range []bool{false, true} {
		script := MariaDBSetupScript("demo_crm", "demo_crm", isDynamic)
		granted := 0
		for _, line := range strings.Split(script, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "GRANT ") {
				continue
			}
			granted++
			// Nothing on the server as a whole, on any database by a bare
			// wildcard, or with the right to grant.
			for _, never := range []string{"*.*", "`%`", "GRANT OPTION", "SUPER", "PROCESS", "FILE", "RELOAD", "CREATE USER", "SHOW DATABASES"} {
				if strings.Contains(line, never) {
					t.Errorf("dynamic=%v: provisioning grants %s: %s", isDynamic, never, line)
				}
			}
		}
		want := MariaDBGrants("demo_crm", "demo_crm", isDynamic)
		if granted != len(want) {
			t.Errorf("dynamic=%v: %d GRANT statements, want %d", isDynamic, granted, len(want))
		}
		for _, grant := range want {
			if !strings.Contains(script, "  "+grant+";\n") {
				t.Errorf("dynamic=%v: provisioning does not issue %s", isDynamic, grant)
			}
		}
		if strings.Contains(script, "*.*") {
			t.Errorf("dynamic=%v: provisioning names *.*:\n%s", isDynamic, script)
		}
		// A user that holds anything else -- as every user provisioned with
		// ALL PRIVILEGES ON *.* does -- has it all taken away before the
		// grants are issued again.
		revoke := strings.Index(script, MariaDBRevokeAll("demo_crm")+";")
		if revoke < 0 || revoke > strings.Index(script, want[0]) {
			t.Errorf("dynamic=%v: provisioning does not revoke what the user must not hold before it grants", isDynamic)
		}
		if !strings.Contains(script, mariadbStraySQL("demo_crm", "demo_crm", isDynamic)) {
			t.Errorf("dynamic=%v: provisioning does not ask whether the user holds anything else", isDynamic)
		}
		// Only this app's user is ever named in a statement that changes
		// rights.
		for _, line := range strings.Split(script, "\n") {
			for _, verb := range []string{"REVOKE ", "GRANT ", "ALTER USER", "CREATE USER", "DROP USER"} {
				if strings.Contains(line, verb) && strings.Contains(line, "@") &&
					!strings.Contains(line, "'demo_crm'@'%'") && !strings.Contains(line, "''demo_crm''@''%''") {
					t.Errorf("dynamic=%v: a statement changes another account: %s", isDynamic, line)
				}
			}
		}
	}
	// The stray check counts the prefix as held only for an app that is to
	// hold it: one that no longer creates databases loses it.
	if strings.Contains(mariadbStraySQL("demo_crm", "demo_crm", false), `demo\_crm\_%`) {
		t.Error("an app that does not create databases may keep a grant on its prefix")
	}
}

// The rule by name, on names built the way the inventory builds them.
func TestMariaDBOwnershipIsByAnUnderscoreDelimitedPrefix(t *testing.T) {
	tenant := func(name, prefix string) *gentianov1alpha1.Tenant {
		tn := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if prefix != "" {
			tn.Spec.Isolation = &gentianov1alpha1.TenantIsolation{DatabasePrefix: prefix}
		}
		return tn
	}
	crm := DatabaseName(tenant("demo", ""), "crm")
	if crm != "demo_crm" {
		t.Fatalf("database = %q", crm)
	}
	exact, owned := MariaDBDatabasePattern(crm), MariaDBOwnedPattern(crm)
	if exact != `demo\_crm` || owned != `demo\_crm\_%` {
		t.Fatalf("patterns = %q, %q", exact, owned)
	}

	for _, c := range []struct {
		why  string
		name string
		mine bool
	}{
		{"the provisioned database", crm, true},
		{"a database the app made", "demo_crm_reports", true},
		{"one named in the app's own way below the prefix", "demo_crm_a-b c", true},
		{"another app whose name begins with this one's", DatabaseName(tenant("demo", ""), "crm2"), false},
		{"the same app of a tenant whose name begins with this one's", DatabaseName(tenant("demo2", ""), "crm"), false},
		{"a tenant with a hyphen where this name has its underscore", DatabaseName(tenant("de-mo", ""), "crm"), false},
		{"an underscore is not any character", "demoXcrm", false},
		{"nor is it below the prefix", "demoXcrm_reports", false},
		{"nor at the boundary", "demo_crmXreports", false},
		{"a longer name without the boundary", "demo_crmreports", false},
		{"another case is another database", "DEMO_CRM_reports", false},
		{"a tenant's prefix override that ends this app's name", DatabaseName(tenant("x", "demo_c"), "rm2"), false},
		{"a name that only ends like it", "x_demo_crm_reports", false},
	} {
		if got := MariaDBOwns(crm, c.name); got != c.mine {
			t.Errorf("%s: MariaDBOwns(%q, %q) = %v", c.why, crm, c.name, got)
		}
		// The patterns the server is given say the same.
		if got := like(exact, c.name) || like(owned, c.name); got != c.mine {
			t.Errorf("%s: the grant patterns match %q = %v", c.why, c.name, got)
		}
	}
	// What the escaping is for: the bare name, as it used to be granted,
	// also names every database that differs where it has an underscore.
	if !like(crm, "demoXcrm") {
		t.Fatal("the unescaped name was expected to match demoXcrm")
	}
	if got, want := MariaDBDatabasePattern(`a_b%c\d`), `a\_b\%c\\d`; got != want {
		t.Errorf("escaped %q, want %q", got, want)
	}

	// Hyphens become underscores, so a provisioned database can lie under
	// another app's prefix by name: crm-extra in the same tenant, and every
	// app of a tenant called demo-crm. The name does not tell these apart
	// from a database crm made.
	for _, other := range []string{
		DatabaseName(tenant("demo", ""), "crm-extra"),
		DatabaseName(tenant("demo-crm", ""), "extra"),
	} {
		if other != "demo_crm_extra" || !MariaDBOwns(crm, other) {
			t.Fatalf("%q was expected to lie under %q by name", other, crm)
		}
	}
	// So the question the acts ask leaves out what another account holds
	// rights on, and provisioning refuses both ways round.
	heldByAnother := `EXISTS (SELECT 1 FROM mysql.db g WHERE NOT (g.User = 'demo_crm' AND g.Host = '%') AND BINARY s.schema_name LIKE BINARY g.Db)`
	if want := "SELECT s.schema_name FROM information_schema.schemata s\n WHERE BINARY s.schema_name LIKE BINARY 'demo\\_crm\\_%'\n   AND NOT " + heldByAnother; mariadbOwnedSQL(crm, "demo_crm") != want {
		t.Errorf("the ownership question is:\n%s\nwant:\n%s", mariadbOwnedSQL(crm, "demo_crm"), want)
	}
	if !strings.HasSuffix(mariadbOthersSQL(crm, "demo_crm"), "AND "+heldByAnother) {
		t.Errorf("the question of what is another account's is:\n%s", mariadbOthersSQL(crm, "demo_crm"))
	}
	static, dynamic := MariaDBSetupScript(crm, "demo_crm", false), MariaDBSetupScript(crm, "demo_crm", true)
	covered := "AND BINARY 'demo_crm' LIKE BINARY g.Db LIMIT 1"
	for name, script := range map[string]string{"static": static, "dynamic": dynamic} {
		refuse, create := strings.Index(script, covered), strings.Index(script, "CREATE DATABASE IF NOT EXISTS `demo_crm`;")
		if refuse < 0 || create < refuse {
			t.Errorf("%s: provisioning does not refuse a database another account's rights reach before it creates it", name)
		}
		if lock := strings.Index(script, "GET_LOCK('gentian-mariadb-provisioning'"); lock < 0 || lock > refuse {
			t.Errorf("%s: the refusal is not decided under the provisioning lock", name)
		}
	}
	if at := strings.Index(dynamic, mariadbOthersSQL(crm, "demo_crm")+" LIMIT 1"); at < 0 || at > strings.Index(dynamic, "GRANT ") {
		t.Error("provisioning grants a prefix without asking whether another account's database lies under it")
	}
	if strings.Contains(static, "is to create databases") {
		t.Error("an app that creates no databases is refused for what lies under its prefix")
	}
}

// Export, restore and purge ask the inventory's one question, of this app's
// database and user, and take every name they get from the server as data.
func TestMariaDBExportRestoreAndPurgeAgreeOnWhichDatabasesAreAnApps(t *testing.T) {
	scripts := mariadbScripts("demo_crm", "demo_crm")
	question := mariadbOwnedSQL("demo_crm", "demo_crm")
	for _, act := range []string{"purge", "export", "restore"} {
		if !strings.Contains(scripts[act], question) {
			t.Errorf("%s does not ask the inventory's question about which databases are the app's", act)
		}
	}
	for act, script := range scripts {
		if !strings.HasPrefix(script, "set -eu") {
			t.Errorf("%s: the script does not stop at the first failing command", act)
		}
		shell := "sh"
		if strings.Contains(script, "pipefail") {
			shell = "bash"
		}
		if out, err := exec.Command(shell, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %s -n: %v\n%s", act, shell, err, out)
		}
		// SQL is handed to the client as written: a here-document the shell
		// expands would run a database name as a command.
		if strings.Contains(script, "<<SQL") || strings.Contains(script, "<<EOF") {
			t.Errorf("%s: SQL passes through shell expansion", act)
		}
		if strings.Contains(script, "for db in") || strings.Contains(script, "--all-databases") {
			t.Errorf("%s splits database names on white space, or takes every database", act)
		}
		for _, line := range strings.Split(script, "\n") {
			if strings.Contains(line, "|| true") || strings.Contains(line, "2>/dev/null") {
				t.Errorf("%s: a failure is discarded: %s", act, line)
			}
			// A dump or an archive piped into something else reports that
			// something's exit status.
			if strings.Contains(line, "mariadb-dump") && strings.Contains(line, "|") ||
				strings.Contains(line, "gunzip") && strings.Contains(line, "|") {
				t.Errorf("%s: a failure is lost in a pipe: %s", act, line)
			}
		}
	}
	// The acts on the provisioned database alone stay with it.
	for _, act := range []string{"export, provisioned", "restore, provisioned"} {
		if strings.Contains(scripts[act], "information_schema") || strings.Contains(scripts[act], "LIKE") {
			t.Errorf("%s reaches beyond the provisioned database", act)
		}
	}

	// Purge: every database of the app's, each DROP built by the server from
	// the name it holds; then the provisioned one and the user, and a count
	// that must be nought.
	purge := scripts["purge"]
	for _, want := range []string{
		"EXECUTE IMMEDIATE CONCAT('DROP DATABASE `', REPLACE(nm, '`', '``'), '`');",
		"DROP DATABASE IF EXISTS `demo_crm`;", "DROP USER IF EXISTS 'demo_crm'@'%';",
		"are still there", "another account holds rights on it; it was left as it is",
	} {
		if !strings.Contains(purge, want) {
			t.Errorf("the purge is missing %q", want)
		}
	}
	if strings.Index(purge, "DROP USER") < strings.Index(purge, "DROP DATABASE IF EXISTS `demo_crm`") {
		t.Error("the purge drops the user before the databases its rights mark as the app's")
	}

	// Export: one dump per database at one instant each, listed in INDEX.
	export := scripts["export"]
	for _, want := range []string{"--single-transaction", `--result-file="/work/owned/${n}.sql" "${name}"`, "/work/owned/INDEX", "REGEXP '[[:cntrl:]]'"} {
		if !strings.Contains(export, want) {
			t.Errorf("the export is missing %q", want)
		}
	}
	if got := MariaDBOwnedArtefact("demo_crm"); got != "mariadb/demo_crm.owned.tar.gz" {
		t.Errorf("artefact = %q", got)
	}

	// Restore: under the name of the database restored into, replaced, and
	// never over a database that is another account's.
	restore := scripts["restore"]
	for _, want := range []string{
		"SRC='old_crm'", "DB='demo_crm'", `target="${DB}_${name#"${SRC}_"}"`,
		"EXECUTE IMMEDIATE CONCAT('DROP DATABASE IF EXISTS `', REPLACE(@db, '`', '``'), '`');",
		"EXECUTE IMMEDIATE CONCAT('CREATE DATABASE `', REPLACE(@db, '`', '``'), '`');",
		mariadbHeldByAnother("@db", "demo_crm"), "the bundle does not hold it; it was left as it is",
		"which is not named as a database of ${SRC}",
	} {
		if !strings.Contains(restore, want) {
			t.Errorf("the restore is missing %q", want)
		}
	}
}

// A name that is not what the platform derives is never written into SQL.
func TestMariaDBScriptsRefuseANameThatIsNotThePlatforms(t *testing.T) {
	for _, bad := range []string{"demo`crm", "demo'crm", "demo crm", "demo-crm", "demo;crm", "demo%", ""} {
		for act, script := range mariadbScripts(bad, "demo_crm") {
			if strings.HasSuffix(act, "provisioned") {
				continue
			}
			if !strings.Contains(script, "ERROR: invalid MariaDB database name") || !strings.HasSuffix(script, "exit 1\n") ||
				strings.Contains(script, "mariadb ") {
				t.Errorf("%s: database %q is not refused:\n%s", act, bad, script)
			}
			if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err == nil {
				t.Errorf("%s: the refusal of %q exits 0: %s", act, bad, out)
			}
		}
		for act, script := range mariadbScripts("demo_crm", bad) {
			if strings.HasSuffix(act, "provisioned") {
				continue
			}
			if !strings.Contains(script, "ERROR: invalid MariaDB user name") {
				t.Errorf("%s: user %q is not refused", act, bad)
			}
		}
	}
}
