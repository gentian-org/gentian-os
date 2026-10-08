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
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// unitJobs is every Job an export and a restore run, by its constructor, for
// each way a bundle is encrypted and each place it can be. A constructor
// added to this package belongs here: the tests below hold every script of
// every one of them to the same rules.
func unitJobs(t *testing.T) map[string]*batchv1.Job {
	t.Helper()
	out := map[string]*batchv1.Job{}
	for _, external := range []bool{false, true} {
		for _, mode := range []gentianov1alpha1.ExportEncryptionMode{
			gentianov1alpha1.ExportEncryptionRecipient, gentianov1alpha1.ExportEncryptionPassphrase,
		} {
			p := params()
			d := Decryption{Mode: mode, SecretName: "key", SecretKey: "identity"}
			if mode == gentianov1alpha1.ExportEncryptionPassphrase {
				p.Encryption = Encryption{Mode: mode, PassphraseSecret: "key", PassphraseKey: "passphrase"}
				d.SecretKey = "passphrase"
			}
			if external {
				p.Endpoint, p.UploadCredentialsSecret = "https://s3.example.org", "destination"
			}
			manifest, err := ManifestJob(p, &Manifest{SchemaVersion: ManifestSchemaVersion, Tenant: "demo"}, NewBundleInfo("demo", "nightly", "now", p.Encryption))
			if err != nil {
				t.Fatal(err)
			}
			for name, job := range map[string]*batchv1.Job{
				"postgres dump":          PostgresDumpJob(p, "demo_wiki"),
				"postgres owned dump":    PostgresOwnedDumpJob(p, "demo_wiki", "demo_wiki"),
				"mariadb dump":           MariaDBDumpJob(p, "demo_shop"),
				"mariadb owned dump":     MariaDBOwnedDumpJob(p, "demo_shop", "demo_shop"),
				"volume archive":         VolumeArchiveJob(p, "data", []string{"**/cache"}),
				"bucket archive":         S3ArchiveJob(p, "demo-wiki"),
				"realm export":           RealmExportJob(p, "demo"),
				"manifest":               manifest,
				"bundle delete":          BundleDeleteJob(p),
				"postgres restore":       PostgresRestoreJob(p, d, PostgresArtefact("old_wiki"), "demo_wiki"),
				"postgres owned restore": PostgresOwnedRestoreJob(p, d, PostgresOwnedArtefact("old_wiki"), "old_wiki", "demo_wiki"),
				"mariadb restore":        MariaDBRestoreJob(p, d, MariaDBArtefact("old_shop"), "demo_shop"),
				"mariadb owned restore":  MariaDBOwnedRestoreJob(p, d, MariaDBOwnedArtefact("old_shop"), "old_shop", "demo_shop", "demo_shop"),
				"bucket restore":         S3RestoreJob(p, d, S3Artefact("old-wiki"), "demo-wiki", ObjectStorageProvisionContainer("provision-bucket", "demo-wiki", "AK", "SK")),
				"volume restore":         VolumeRestoreJob(p, d, VolumeArtefact("data"), "data"),
				"realm import":           RealmImportJob(p, d, IdentityArtefact, "demo", RealmSource{}),
				"realm import, renamed":  RealmImportJob(p, d, IdentityArtefact, "demo", RealmSource{Tenant: "old", Realm: "old"}),
			} {
				out[fmt.Sprintf("%s (external=%v, %s)", name, external, mode)] = job
			}
		}
	}
	return out
}

// containerScript is what a container runs: these Jobs hand a script to a
// shell, as the last argument or the last word of the command.
func containerScript(c corev1.Container) (shell, script string) {
	words := append(append([]string{}, c.Command...), c.Args...)
	if len(words) < 3 {
		return "", ""
	}
	return words[0], words[len(words)-1]
}

// allowedQuiet are the lines of a unit's script that may hide an error's
// text, each with why. Nothing here discards a failure: every one of them is
// followed by a branch that fails the script with its own message.
var allowedQuiet = []string{
	// Installing a tool: the package manager's chatter is hidden, and a
	// failure ends in "could not install" or "<tool> unavailable".
	"apk add --no-cache --quiet",
	`command -v "${tool}" >/dev/null 2>&1 ||`,
	// The step install provisions a bucket with, run before a bucket is
	// filled: it takes away the user and the policy it is about to make
	// again, which are not there on a first run. A store that cannot be
	// reached fails the commands that make them, on the next lines.
	`mc admin user remove gentian "${APP_ACCESS_KEY}" >/dev/null 2>&1 || true`,
	`mc admin policy rm gentian "${APP_ACCESS_KEY}-policy" >/dev/null 2>&1 || true`,
}

func allowedLine(line string) bool {
	for _, ok := range allowedQuiet {
		if strings.Contains(line, ok) {
			return true
		}
	}
	return false
}

// An export and a restore report success only for what they did. Every
// script of every unit stops at the first command that fails and discards no
// failure: no "|| true", no "|| echo", no error hidden without a branch that
// fails. And every one of them parses -- a Job that fails at `sh -n` is found
// by nobody until a restore.
//
// The destroy scripts were held to this (TestTheDestroyScriptsDiscardNoFailure)
// and these were not. The realm export recorded a read that failed as "no
// groups", and the realm import passed over a person it could not create and
// ended each membership with "|| true": a bundle and a restore that looked
// complete and lacked people.
func TestNoUnitScriptDiscardsAFailure(t *testing.T) {
	for name, job := range unitJobs(t) {
		containers := append(append([]corev1.Container{}, job.Spec.Template.Spec.InitContainers...), job.Spec.Template.Spec.Containers...)
		for _, c := range containers {
			where := name + "/" + c.Name
			shell, script := containerScript(c)
			if script == "" {
				t.Errorf("%s: runs no script this test can read", where)
				continue
			}
			body := strings.TrimPrefix(script, keycloak.ProvisionerBootstrap)
			if !strings.HasPrefix(body, "set -eu") {
				t.Errorf("%s: the script does not stop at the first failing command", where)
			}
			for _, line := range strings.Split(script, "\n") {
				if allowedLine(line) {
					continue
				}
				if strings.Contains(line, "|| true") || strings.Contains(line, "|| echo") || strings.Contains(line, "|| :") {
					t.Errorf("%s: a failure is discarded: %s", where, line)
				}
				if strings.Contains(line, "2>/dev/null") || strings.Contains(line, ">/dev/null 2>&1") {
					t.Errorf("%s: an error is hidden: %s", where, line)
				}
			}
			parser := "sh"
			if strings.HasSuffix(shell, "bash") {
				parser = "bash"
			}
			if out, err := exec.Command(parser, "-n", "-c", script).CombinedOutput(); err != nil {
				t.Errorf("%s: %s -n: %v\n%s", where, parser, err, out)
			}
		}
	}
}

// realmWorld runs a realm script against an identity provider made of files:
// curl is a stub that answers by method and path from a directory and
// records what it was asked.
type realmWorld struct {
	t    *testing.T
	dir  string
	work string
}

func newRealmWorld(t *testing.T) *realmWorld {
	t.Helper()
	for _, tool := range []string{"jq", "tar", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	w := &realmWorld{t: t, dir: t.TempDir()}
	w.work = filepath.Join(w.dir, "work")
	for _, d := range []string{"bin", "work", "answers"} {
		if err := os.MkdirAll(filepath.Join(w.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// apk: nothing to install here.
	w.write("bin/apk", "#!/bin/sh\nexit 0\n")
	// curl: the token, or the answer on file for "<METHOD> <path>" with the
	// status on its first line; 404 for anything not on file. Every request
	// is appended to the log, with the body it carried.
	w.write("bin/curl", `#!/bin/sh
method=GET; out=/dev/stdout; data=; url=; fmt=
while [ $# -gt 0 ]; do
  case "$1" in
    -X) method="$2"; shift ;;
    -o) out="$2"; shift ;;
    -w) fmt="$2"; shift ;;
    --data) data="${2#@}"; shift ;;
    -H|--max-time|--data-urlencode) shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
case "${url}" in
  */protocol/openid-connect/token) printf '{"access_token":"t"}'; exit 0 ;;
esac
path="${url#http://kc/admin/realms/}"
echo "${method} ${path}" >> "${STUB}/log"
[ -z "${data}" ] || { echo "BODY ${method} ${path} $(tr -d '\n' < "${data}")" >> "${STUB}/log"; }
key="$(printf '%s %s' "${method}" "${path}" | tr '/?&=%:' '______')"
if [ -f "${STUB}/answers/${key}" ]; then
  code="$(head -n 1 "${STUB}/answers/${key}")"
  tail -n +2 "${STUB}/answers/${key}" > "${out}"
else
  code=404
  printf '{"error":"not found"}' > "${out}"
fi
[ -z "${fmt}" ] || printf '%s' "${code}"
exit 0
`)
	return w
}

func (w *realmWorld) write(rel, content string) {
	w.t.Helper()
	path := filepath.Join(w.dir, rel)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

// answer puts an answer on file for a request.
func (w *realmWorld) answer(method, path string, code int, body string) {
	key := strings.NewReplacer("/", "_", "?", "_", "&", "_", "=", "_", "%", "_", ":", "_").Replace(method + " " + path)
	w.write("answers/"+key, fmt.Sprintf("%d\n%s", code, body))
}

// run runs a container's script with the stubs, its scratch mount moved to a
// directory of the test's.
func (w *realmWorld) run(script string) (string, error) {
	w.t.Helper()
	script = strings.ReplaceAll(script, workDir+"/", w.work+"/")
	script = strings.ReplaceAll(script, "-C "+workDir, "-C "+w.work)
	script = strings.ReplaceAll(script, "/tmp/kc.answer", filepath.Join(w.dir, "kc.answer"))
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{
		"PATH=" + filepath.Join(w.dir, "bin") + ":" + os.Getenv("PATH"),
		"STUB=" + w.dir, "KEYCLOAK_URL=http://kc",
		"KEYCLOAK_ADMIN_USERNAME=admin", "KEYCLOAK_ADMIN_PASSWORD=p&ss word",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// compactJSON takes the spaces jq prints out of a logged body.
func compactJSON(s string) string {
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	for _, pair := range [][2]string{{`": `, `":`}, {"{ ", "{"}, {" }", "}"}, {"[ ", "["}, {" ]", "]"}, {", ", ","}} {
		s = strings.ReplaceAll(s, pair[0], pair[1])
	}
	return s
}

func (w *realmWorld) log() string {
	raw, _ := os.ReadFile(filepath.Join(w.dir, "log"))
	return string(raw)
}

// archive packs what a realm export wrote, as the import's fetch step leaves it.
func (w *realmWorld) archive(realm, users, memberships string) {
	w.t.Helper()
	for name, content := range map[string]string{"realm.json": realm, "users.ndjson": users, "memberships.ndjson": memberships} {
		if err := os.WriteFile(filepath.Join(w.work, name), []byte(content), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
	if out, err := exec.Command("tar", "czf", filepath.Join(w.work, "realm.tar.gz"), "-C", w.work,
		"realm.json", "users.ndjson", "memberships.ndjson").CombinedOutput(); err != nil {
		w.t.Fatalf("tar: %v\n%s", err, out)
	}
	for _, name := range []string{"realm.json", "users.ndjson", "memberships.ndjson"} {
		_ = os.Remove(filepath.Join(w.work, name))
	}
}

func importScript(tenant string, source RealmSource) string {
	p := params()
	p.Tenant = tenant
	return containerByName(RealmImportJob(p, recipientDecryption(), IdentityArtefact, tenant, source), "realm-import").Args[0]
}

const (
	bundleRealm = `{"realm":"acme",
 "groups":[{"name":"gentian:tenant:acme:members","path":"/gentian:tenant:acme:members","clientRoles":{"acme-wiki":["user"]}},
           {"name":"staff","path":"/staff","subGroups":[{"name":"board","path":"/staff/board"}]}],
 "roles":{"realm":[{"name":"default-roles-acme"},{"name":"auditor"}],"client":{"acme-wiki":[{"name":"user"}]}},
 "clients":[{"clientId":"acme-wiki","redirectUris":["https://wiki.acme.example/*"]}]}`
	bundleUsers = `{"id":"u1","username":"ada","email":"ada@example.org"}
{"id":"u2","username":"service-account-acme-wiki","serviceAccountClientId":"acme-wiki"}
{"id":"u3","username":"bob"}
`
	bundleMemberships = `{"userId":"u1","groups":[{"path":"/gentian:tenant:acme:members"},{"path":"/staff/board"}]}
{"userId":"u2","groups":[]}
{"userId":"u3","groups":[{"path":"/gentian:tenant:acme:members"}]}
`
)

// A bundle imported under another name beside the tenant it was taken of:
// the platform's groups come back under the new tenant's names with their
// members, and nothing named for the old tenant is planted in the new realm
// -- no client, no client role, no mapping to one.
func TestARealmImportedUnderAnotherNameIsRenamed(t *testing.T) {
	w := newRealmWorld(t)
	w.archive(bundleRealm, bundleUsers, bundleMemberships)
	w.answer("POST", "acme2/partialImport", 200, `{}`)
	w.answer("GET", "acme2/users?exact=true&username=ada", 200, `[]`)
	w.answer("GET", "acme2/users?exact=true&username=bob", 200, `[{"id":"n3"}]`)
	w.answer("POST", "acme2/users", 201, ``)
	w.answer("GET", "acme2/group-by-path/gentian%3Atenant%3Aacme2%3Amembers", 200, `{"id":"g-members"}`)
	w.answer("GET", "acme2/group-by-path/staff/board", 200, `{"id":"g-board"}`)
	w.answer("PUT", "acme2/users/n3/groups/g-members", 204, ``)

	// ada does not exist yet: she is created, and then has to be found.
	out, err := w.run(importScript("acme2", RealmSource{Tenant: "acme", Realm: "acme"}))
	log := w.log()
	// She was created and the lookup still answers "nobody": that is a
	// membership that cannot be put back, and the import must say so.
	if err == nil || !strings.Contains(out, "ada is not in the realm") {
		t.Fatalf("a person the realm does not have after the import is not an error:\n%s", out)
	}

	var imported string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "BODY POST acme2/partialImport ") {
			imported = compactJSON(line)
		}
	}
	for _, want := range []string{`"name":"gentian:tenant:acme2:members"`, `"path":"/gentian:tenant:acme2:members"`, `"name":"staff"`, `"auditor"`} {
		if !strings.Contains(imported, want) {
			t.Errorf("the import does not carry %s:\n%s", want, imported)
		}
	}
	for _, not := range []string{"gentian:tenant:acme:", "acme-wiki", "clientRoles", `"clients"`, "default-roles-acme", "wiki.acme.example"} {
		if strings.Contains(imported, not) {
			t.Errorf("the import carries %q, which is the old tenant's:\n%s", not, imported)
		}
	}
	if strings.Contains(log, "service-account") {
		t.Errorf("a client's service account was looked up or created as a person:\n%s", log)
	}
	if !strings.Contains(log, "PUT acme2/users/n3/groups/g-members") {
		t.Errorf("bob was not put back into the renamed group:\n%s", log)
	}
	if strings.Contains(log, "acme/") || strings.Contains(log, "acme%3Amembers") {
		t.Errorf("the original tenant's realm or groups were addressed:\n%s", log)
	}
}

// The same bundle into the tenant it was taken of: names as they were,
// clients and all, and everyone back in their groups.
func TestARealmRestoredIntoItsOwnTenantKeepsItsNames(t *testing.T) {
	w := newRealmWorld(t)
	w.archive(bundleRealm, bundleUsers, bundleMemberships)
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.answer("GET", "acme/users?exact=true&username=ada", 200, `[{"id":"n1"}]`)
	w.answer("GET", "acme/users?exact=true&username=bob", 200, `[{"id":"n3"}]`)
	w.answer("GET", "acme/group-by-path/gentian%3Atenant%3Aacme%3Amembers", 200, `{"id":"g-members"}`)
	w.answer("GET", "acme/group-by-path/staff/board", 200, `{"id":"g-board"}`)
	w.answer("PUT", "acme/users/n1/groups/g-members", 204, ``)
	w.answer("PUT", "acme/users/n1/groups/g-board", 204, ``)
	w.answer("PUT", "acme/users/n3/groups/g-members", 204, ``)

	out, err := w.run(importScript("acme", RealmSource{Tenant: "acme", Realm: "acme"}))
	if err != nil {
		t.Fatalf("the import failed: %v\n%s\n%s", err, out, w.log())
	}
	log := compactJSON(w.log())
	for _, want := range []string{`"clientId":"acme-wiki"`, `"name":"gentian:tenant:acme:members"`,
		"PUT acme/users/n1/groups/g-board", "PUT acme/users/n3/groups/g-members"} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %s:\n%s", want, log)
		}
	}
	if !strings.Contains(out, "WITHOUT credentials") {
		t.Errorf("the output does not say members have no credentials:\n%s", out)
	}
}

// What the import used to pass over in silence now fails it, with the name:
// a person the provider refuses, a group that is not there, a membership
// that is refused -- and it still goes to the end, so the output names all.
func TestARealmImportFailsForWhatItCouldNotPutBack(t *testing.T) {
	w := newRealmWorld(t)
	w.archive(bundleRealm, bundleUsers, bundleMemberships)
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.answer("GET", "acme/users?exact=true&username=ada", 200, `[]`)
	w.answer("POST", "acme/users", 409, `{"errorMessage":"User exists with same email"}`)
	w.answer("GET", "acme/users?exact=true&username=bob", 200, `[{"id":"n3"}]`)
	// The members group is not there; nothing answers for it.

	out, err := w.run(importScript("acme", RealmSource{}))
	if err == nil {
		t.Fatalf("the import reported success:\n%s", out)
	}
	for _, want := range []string{
		"answered 409", "User exists with same email", "ada could not be created",
		"bob was a member of /gentian:tenant:acme:members, which is not in the realm",
		"could not be put back; each is named above",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "restored 0 user(s) WITHOUT") {
		t.Errorf("the import ended with its sentence of success:\n%s", out)
	}
}

// A realm export whose read of one person's groups fails is a failed export.
// It used to write "no groups" for that person and upload the bundle.
func TestARealmExportFailsWhenAMembershipCannotBeRead(t *testing.T) {
	w := newRealmWorld(t)
	script := initContainerScript(RealmExportJob(params(), "acme"))
	w.answer("POST", "acme/partial-export?exportGroupsAndRoles=true&exportClients=true", 200, `{"realm":"acme"}`)
	w.answer("GET", "acme/users?briefRepresentation=false&first=0&max=100", 200, `[{"id":"u1","username":"ada"},{"id":"u2","username":"bob"}]`)
	w.answer("GET", "acme/users/u1/groups", 200, `[{"path":"/staff"}]`)
	w.answer("GET", "acme/users/u2/groups", 500, `{"error":"unknown_error"}`)

	out, err := w.run(script)
	if err == nil {
		t.Fatalf("the export reported success with a membership it could not read:\n%s", out)
	}
	if !strings.Contains(out, "users/u2/groups answered 500") {
		t.Errorf("the output does not say what failed:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(w.work, "realm.tar.gz")); statErr == nil {
		t.Error("an archive was written for a realm that was not read to the end")
	}

	// And with every read answered, the archive holds each person's groups.
	w.answer("GET", "acme/users/u2/groups", 200, `[]`)
	if out, err := w.run(script); err != nil {
		t.Fatalf("the export failed: %v\n%s", err, out)
	}
	listing, err := exec.Command("tar", "xzOf", filepath.Join(w.work, "realm.tar.gz"), "memberships.ndjson").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, listing)
	}
	if got := strings.TrimSpace(string(listing)); got != `{"userId":"u1","groups":[{"path":"/staff"}]}`+"\n"+`{"userId":"u2","groups":[]}` {
		t.Errorf("memberships = %s", got)
	}
}

// ownedWorld runs the restore of the databases an app's role owned against
// a PostgreSQL made of files: psql answers who owns a database from a table
// and records what it was told to create; pg_restore writes an empty script.
func runOwnedRestore(t *testing.T, source, database string, index []string, owners map[string]string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(work, "owned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "owned", "INDEX"), []byte(strings.Join(index, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range index {
		if err := os.WriteFile(filepath.Join(work, "owned", fmt.Sprintf("%d.pgc", i)), []byte("dump"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var table strings.Builder
	for db, owner := range owners {
		fmt.Fprintf(&table, "%s %s\n", db, owner)
	}
	if err := os.WriteFile(filepath.Join(dir, "owners"), []byte(table.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// psql: "-v db=<name>" with the owner question on stdin answers from the
	// table; with CREATE DATABASE on stdin it records the name; a load
	// (PGDATABASE set, -f) records the database loaded into.
	psql := `#!/bin/sh
db=
for a in "$@"; do case "$a" in db=*) db="${a#db=}" ;; esac; done
sql="$(cat 2>/dev/null || true)"
case "${sql}" in
  *"FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba WHERE d.datname"*)
    awk -v d="${db}" '$1 == d { print $2 }' "${STUB}/owners" ;;
  *"CREATE DATABASE"*) echo "CREATE ${db}" >> "${STUB}/log" ;;
esac
[ -z "${PGDATABASE:-}" ] || echo "LOAD ${PGDATABASE}" >> "${STUB}/log"
exit 0
`
	for name, content := range map[string]string{"psql": psql, "pg_restore": "#!/bin/sh\nfor a in \"$@\"; do case \"$prev\" in -f) : > \"$a\" ;; esac; prev=\"$a\"; done\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := params()
	p.Tenant, p.App = "acme2", "wiki"
	script := containerByName(PostgresOwnedRestoreJob(p, recipientDecryption(), "postgres/x.owned.tar.gz", source, database), "pg-restore-owned").Args[0]
	script = strings.ReplaceAll(script, "/tmp/", "@TMP@/")
	script = strings.ReplaceAll(script, workDir+"/", work+"/")
	script = strings.ReplaceAll(script, "@TMP@/", dir+"/")
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = strings.NewReader("")
	cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "STUB=" + dir}
	out, err := cmd.CombinedOutput()
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	return string(out) + "\n--- log\n" + string(log), err
}

// One PostgreSQL server holds every tenant's databases, and a database an
// app made for itself is named whatever the app chose. A bundle imported
// under another name beside the tenant it was taken of used to put each back
// under its old name: the original's database, created if missing and
// otherwise REPLACED. Now each lands under the new tenant's own database
// name; and whatever the naming does, a database another role owns is never
// replaced.
func TestOwnedDatabasesAreRestoredUnderTheTargetsNamesAndNeverOverAnothers(t *testing.T) {
	// Under another name: nothing of the original is addressed.
	out, err := runOwnedRestore(t, "acme_wiki", "acme2_wiki",
		[]string{"acme_wiki_reports", "analytics"},
		map[string]string{"acme_wiki_reports": "acme_wiki", "analytics": "acme_wiki"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"restored acme_wiki_reports as acme2_wiki_reports", "restored analytics as acme2_wiki_analytics",
		"CREATE acme2_wiki_reports", "LOAD acme2_wiki_reports", "LOAD acme2_wiki_analytics",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"LOAD acme_wiki_reports", "LOAD analytics", "CREATE analytics"} {
		if strings.Contains(out, not) {
			t.Errorf("the original's database was written to (%s):\n%s", not, out)
		}
	}

	// Into its own tenant: each under its own name.
	out, err = runOwnedRestore(t, "acme_wiki", "acme_wiki", []string{"analytics"}, map[string]string{"analytics": "acme2_wiki"})
	if err != nil || !strings.Contains(out, "restored analytics as analytics") {
		t.Errorf("a restore into the same tenant renamed a database: %v\n%s", err, out)
	}

	// A name another role holds is refused, and nothing is loaded into it.
	out, err = runOwnedRestore(t, "acme_wiki", "acme_wiki", []string{"analytics"}, map[string]string{"analytics": "globex_crm"})
	if err == nil || !strings.Contains(out, "refused") || !strings.Contains(out, "a database globex_crm owns") || strings.Contains(out, "LOAD") {
		t.Errorf("another role's database was not refused: %v\n%s", err, out)
	}

	// A name too long to be one is refused rather than cut short.
	long := strings.Repeat("x", 60)
	out, err = runOwnedRestore(t, "acme_wiki", "acme2_wiki", []string{long}, nil)
	if err == nil || !strings.Contains(out, "longer than a database name may be") {
		t.Errorf("a name PostgreSQL would cut short was accepted: %v\n%s", err, out)
	}
}
