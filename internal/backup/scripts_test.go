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
	"slices"
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
			manifest, err := ManifestJob(p, &Manifest{SchemaVersion: ManifestSchemaVersion, Tenant: "demo"}, NewBundleInfo("demo", "nightly", "now", p.Encryption), nil)
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
				"bucket restore":         S3RestoreJob(p, d, S3Artefact("old-wiki"), "demo-wiki", ObjectStorageProvisionContainer("provision-bucket", "demo-wiki", "key-secret")),
				"volume restore":         VolumeRestoreJob(p, d, VolumeArtefact("data"), "data"),
				"realm import":           RealmImportJob(p, d, IdentityArtefact, "demo", RealmSource{}),
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
seen=$(( $(cat "${STUB}/answers/${key}.count" 2>/dev/null || echo 0) + 1 ))
echo "${seen}" > "${STUB}/answers/${key}.count"
[ ! -f "${STUB}/answers/${key}.${seen}" ] || key="${key}.${seen}"
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

// clientsHere answers the import's read of the clients the realm has.
func (w *realmWorld) clientsHere(clients string) {
	w.answer("GET", "acme/clients?first=0&max=100000", 200, clients)
}

// groupsHere answers for bundleRealm's groups as the realm has them: each
// is here, under an id of its own, and its role at acme-wiki is too.
func (w *realmWorld) groupsHere() {
	w.answer("GET", "acme/group-by-path/gentian%3Atenant%3Aacme%3Amembers", 200,
		`{"id":"g-members","name":"gentian:tenant:acme:members","attributes":{"gentianos.io/managed-by":["platform"]}}`)
	w.answer("GET", "acme/group-by-path/staff", 200, `{"id":"g-staff","name":"staff"}`)
	w.answer("GET", "acme/group-by-path/staff/board", 200, `{"id":"g-board","name":"board"}`)
	w.answer("PUT", "acme/groups/g-staff", 204, ``)
	w.answer("PUT", "acme/groups/g-board", 204, ``)
	w.answer("GET", "acme/clients?clientId=acme-wiki", 200, `[{"id":"k1","clientId":"acme-wiki"}]`)
	w.answer("GET", "acme/clients/k1/roles/user", 200, `{"id":"r-user","name":"user"}`)
	w.answer("POST", "acme/groups/g-members/role-mappings/clients/k1", 204, ``)
}

// answerNth puts an answer on file for the nth time a request is made; the
// other times get the one answer gives.
func (w *realmWorld) answerNth(method, path string, nth, code int, body string) {
	key := strings.NewReplacer("/", "_", "?", "_", "&", "_", "=", "_", "%", "_", ":", "_").Replace(method + " " + path)
	w.write(fmt.Sprintf("answers/%s.%d", key, nth), fmt.Sprintf("%d\n%s", code, body))
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
		"KEYCLOAK_CLIENT_ID=admin", "KEYCLOAK_CLIENT_SECRET=p&ss word",
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

// A bundle into the tenant it was taken of: names as they were, the groups
// that are here kept with the roles the bundle says they held, and everyone
// back in their groups.
func TestARealmRestoredIntoItsOwnTenantKeepsItsNames(t *testing.T) {
	w := newRealmWorld(t)
	w.archive(bundleRealm, bundleUsers, bundleMemberships)
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.clientsHere(`[{"id":"k1","clientId":"acme-wiki"}]`)
	w.groupsHere()
	w.answer("GET", "acme", 200, `{"realm":"acme","defaultRole":{"name":"default-roles-acme"}}`)
	w.answer("GET", "acme/users?exact=true&username=ada", 200, `[{"id":"n1"}]`)
	w.answer("GET", "acme/users?exact=true&username=bob", 200, `[{"id":"n3"}]`)
	w.answer("GET", "acme/group-by-path/gentian%3Atenant%3Aacme%3Amembers", 200, `{"id":"g-members"}`)
	w.answer("GET", "acme/group-by-path/staff/board", 200, `{"id":"g-board"}`)
	w.answer("PUT", "acme/users/n1/groups/g-members", 204, ``)
	w.answer("PUT", "acme/users/n1/groups/g-board", 204, ``)
	w.answer("PUT", "acme/users/n3/groups/g-members", 204, ``)

	out, err := w.run(importScript("acme", RealmSource{}))
	if err != nil {
		t.Fatalf("the import failed: %v\n%s\n%s", err, out, w.log())
	}
	log := compactJSON(w.log())
	for _, want := range []string{`"clientId":"acme-wiki"`, "POST acme/groups/g-members/role-mappings/clients/k1",
		`BODY POST acme/groups/g-members/role-mappings/clients/k1 [{"id":"r-user","name":"user"}]`,
		"PUT acme/users/n1/groups/g-board", "PUT acme/users/n3/groups/g-members"} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, "OVERWRITE") {
		t.Errorf("the import overwrites something the realm has:\n%s", log)
	}
	if !strings.Contains(out, "WITHOUT credentials") {
		t.Errorf("the output does not say members have no credentials:\n%s", out)
	}
}

// People come back under the ids the bundle recorded, so an app that keys
// an account by the token's subject finds the same person. Each is created
// by a partial import of that one person, with the realm's own default role
// and their links to other providers, as before; nothing else the bundle
// might carry for them: no credential, no role or group by name (a partial
// import would make up a role it does not find). A person who is there already keeps
// the id they have, and the output says so. An id that is not a UUID is not
// kept: it would become a subject the rights store knows people by.
func TestARealmImportKeepsPeoplesIds(t *testing.T) {
	w := newRealmWorld(t)
	users := `{"id":"0c4f1b2e-7a3d-4e8f-9b1a-2d3c4e5f6a7b","username":"ada","email":"ada@example.org","firstName":"Ada","attributes":{"locale":["en"]},` +
		`"createdTimestamp":1,"credentials":[{"type":"password","value":"x"}],"realmRoles":["admin"],` +
		`"clientRoles":{"realm-management":["realm-admin"]},"groups":["/staff"],"federationLink":"ldap",` +
		`"federatedIdentities":[{"identityProvider":"corp","userId":"9","userName":"ada"}]}
{"id":"u3","username":"bob"}
{"id":"f:ldap:carol","username":"carol"}
`
	w.archive(bundleRealm, users, `{"userId":"0c4f1b2e-7a3d-4e8f-9b1a-2d3c4e5f6a7b","groups":[]}
{"userId":"u3","groups":[]}
{"userId":"f:ldap:carol","groups":[]}
`)
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.clientsHere(`[{"id":"k1","clientId":"acme-wiki"}]`)
	w.groupsHere()
	w.answer("GET", "acme/users?exact=true&username=carol", 200, `[]`)
	w.answer("GET", "acme", 200, `{"realm":"acme","defaultRole":{"name":"default-roles-acme"}}`)
	w.answer("GET", "acme/users?exact=true&username=ada", 200, `[]`)
	w.answer("GET", "acme/users?exact=true&username=bob", 200, `[{"id":"n3"}]`)

	out, err := w.run(importScript("acme", RealmSource{}))
	if err != nil {
		t.Fatalf("the import failed: %v\n%s\n%s", err, out, w.log())
	}
	log := w.log()
	if strings.Contains(log, "POST acme/users") {
		t.Errorf("a person was created through POST /users, which gives a new id:\n%s", log)
	}
	var people []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "BODY POST acme/partialImport ") && strings.Contains(line, `"users"`) {
			people = append(people, compactJSON(line))
		}
	}
	if len(people) != 2 {
		t.Fatalf("want two people imported (ada and carol; bob is there), got %d:\n%s", len(people), log)
	}
	// An id that is not a UUID Keycloak would give is not kept: carol gets a
	// new one, and the output says so.
	if strings.Contains(people[1], `"id"`) || !strings.Contains(people[1], `"username":"carol"`) {
		t.Errorf("carol's import keeps an id that is not a UUID:\n%s", people[1])
	}
	if !strings.Contains(out, "carol had the id f:ldap:carol") {
		t.Errorf("the output does not say carol gets a new id:\n%s", out)
	}
	ada := people[0]
	for _, want := range []string{`"ifResourceExists":"FAIL"`, `"id":"0c4f1b2e-7a3d-4e8f-9b1a-2d3c4e5f6a7b"`, `"username":"ada"`, `"email":"ada@example.org"`,
		`"firstName":"Ada"`, `"locale":["en"]`, `"enabled":true`, `"realmRoles":["default-roles-acme"]`,
		`"federatedIdentities":[{"identityProvider":"corp","userId":"9","userName":"ada"}]`} {
		if !strings.Contains(ada, want) {
			t.Errorf("ada's import does not carry %s:\n%s", want, ada)
		}
	}
	for _, not := range []string{"credentials", `"admin"`, "clientRoles", "realm-admin", "/staff", "federationLink", "createdTimestamp"} {
		if strings.Contains(ada, not) {
			t.Errorf("ada's import carries %s:\n%s", not, ada)
		}
	}
	if !strings.Contains(out, "bob is already in the realm under id n3, not u3") {
		t.Errorf("the output does not say bob keeps another id:\n%s", out)
	}
	if !strings.Contains(out, "restored 2 user(s) WITHOUT credentials") {
		t.Errorf("the output does not count ada and carol or warn about credentials:\n%s", out)
	}
}

// A tenant whose mail domain is another than where the bundle was taken gets
// its people back with their addresses at the new one: the same name before
// the @, and a username that was the address with it. An address at another
// domain -- a subdomain of the old one among them -- is the person's own and
// is not touched; a person who is there already under the new address is
// left as they are, and their groups are put back by that name.
func TestARealmImportMovesAddressesToTheNewMailDomain(t *testing.T) {
	w := newRealmWorld(t)
	users := `{"id":"u1","username":"ada","email":"Ada@Acme.Example.Org"}
{"id":"u2","username":"admin@acme.example.org","email":"admin@acme.example.org"}
{"id":"u3","username":"carol@elsewhere.example","email":"carol@elsewhere.example"}
{"id":"u4","username":"dan","email":"dan@mail.acme.example.org"}
{"id":"u5","username":"eve"}
`
	w.archive(bundleRealm, users, `{"userId":"u2","groups":[{"path":"/staff/board"}]}
`)
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.clientsHere(`[{"id":"k1","clientId":"acme-wiki"}]`)
	w.groupsHere()
	w.answer("GET", "acme", 200, `{"realm":"acme","defaultRole":{"name":"default-roles-acme"}}`)
	for _, name := range []string{"ada", "carol%40elsewhere.example", "dan", "eve"} {
		w.answer("GET", "acme/users?exact=true&username="+name, 200, `[]`)
	}
	// The administrator is made with the tenant, under the new address.
	w.answer("GET", "acme/users?exact=true&username=admin%40acme.example.net", 200, `[{"id":"n2"}]`)
	w.answer("GET", "acme/group-by-path/staff/board", 200, `{"id":"g-board"}`)
	w.answer("PUT", "acme/users/n2/groups/g-board", 204, ``)

	out, err := w.run(importScript("acme", RealmSource{MailDomain: "acme.example.org", TargetMailDomain: "Acme.Example.NET"}))
	if err != nil {
		t.Fatalf("the import failed: %v\n%s\n%s", err, out, w.log())
	}
	log := compactJSON(w.log())
	var people []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "BODY POST acme/partialImport ") && strings.Contains(line, `"users"`) {
			people = append(people, line)
		}
	}
	if len(people) != 4 {
		t.Fatalf("want ada, carol, dan and eve created, got %d:\n%s", len(people), log)
	}
	for i, want := range [][]string{
		{`"username":"ada"`, `"email":"Ada@acme.example.net"`},
		{`"username":"carol@elsewhere.example"`, `"email":"carol@elsewhere.example"`},
		{`"username":"dan"`, `"email":"dan@mail.acme.example.org"`},
		{`"username":"eve"`},
	} {
		for _, w := range want {
			if !strings.Contains(people[i], w) {
				t.Errorf("person %d does not carry %s:\n%s", i, w, people[i])
			}
		}
	}
	if strings.Contains(log, "admin%40acme.example.org") || strings.Contains(log, `"admin@acme.example.org"`) {
		t.Errorf("the administrator was looked up or created under the old address:\n%s", log)
	}
	if !strings.Contains(log, "PUT acme/users/n2/groups/g-board") {
		t.Errorf("the administrator's group was not put back by the new address:\n%s", log)
	}
	if !strings.Contains(out, "the addresses at acme.example.org are put back at acme.example.net, the tenant's mail domain here: 2 person(s)") {
		t.Errorf("the output does not say how many addresses moved:\n%s", out)
	}

	// The same domain, an unknown one, or one that is not a domain's name:
	// nothing moves.
	for _, source := range []RealmSource{
		{MailDomain: "acme.example.org", TargetMailDomain: "ACME.example.org"},
		{TargetMailDomain: "acme.example.net"},
		{MailDomain: "acme.example.org"},
		{MailDomain: "acme.example.org", TargetMailDomain: "x'; rm -rf /; '"},
		{MailDomain: "a+b:c", TargetMailDomain: "acme.example.net"},
	} {
		if from, to := source.mailMove(); from != "" || to != "" {
			t.Errorf("%+v moves %q to %q", source, from, to)
		}
		if script := importScript("acme", source); !strings.Contains(script, "MAIL_FROM=''\nMAIL_TO=''") {
			t.Errorf("%+v sets the domains in the script", source)
		}
	}
}

// partialImports are the bodies of the partial imports a run posted, in order.
func (w *realmWorld) partialImports() []string {
	var bodies []string
	for _, line := range strings.Split(compactJSON(w.log()), "\n") {
		if strings.HasPrefix(line, "BODY POST acme/partialImport ") {
			bodies = append(bodies, strings.TrimPrefix(line, "BODY POST acme/partialImport "))
		}
	}
	return bodies
}

// The bundle's clients are never put over the realm's, and the platform's
// are never made from it. Keycloak's export masks a client's secret
// ("**********"), and OVERWRITE deletes the client and makes it anew from the
// bundle: under a new id, at the addresses the bundle was taken at, with the
// mask as its secret. So clients and roles go in one partial import with
// SKIP, roles of the realm among them (OVERWRITE would make each anew, held
// by nobody); a client of the tenant's that is not here comes with its roles
// and no masked value, so it gets a new secret.
//
// A client the platform declares is left out even where it is not here: its
// provider-keycloak resource makes it, and could not once a copy from the
// bundle stood under its name. It is known by the platform's mark, or, in a
// bundle taken before the mark, by its name: a pattern of the platform's
// own names, or an app's clientId the catalog names. Its roles are left
// out with it, as Keycloak refuses an import that names a client's roles
// and not the client; and a group's role at it is said, not granted.
func TestARealmImportNeverPutsTheBundlesClientsOverTheRealms(t *testing.T) {
	w := newRealmWorld(t)
	realm := `{"realm":"acme",
 "groups":[{"name":"staff","path":"/staff","clientRoles":{"gentian-xwiki":["user"],"acme-tool":["user"]}}],
 "roles":{"realm":[{"name":"auditor","composites":{"client":{"gentian-xwiki":["user"],"acme-tool":["user"]}}}],
          "client":{"acme-wiki":[{"name":"user"}],"acme-tool":[{"name":"user"}],"gentian-xwiki":[{"name":"user"}],
                    "acme-marked":[{"name":"user"}],"realm-management":[{"name":"realm-admin"}]}},
 "clients":[{"id":"c1","clientId":"acme-wiki","secret":"**********","registrationAccessToken":"**********",
             "redirectUris":["https://wiki.old.example/*"],
             "attributes":{"saml.signing.certificate.private.key":"**********","post.logout.redirect.uris":"+"}},
            {"id":"c2","clientId":"acme-tool","publicClient":true},
            {"id":"c3","clientId":"acme-marked","attributes":{"gentianos.io/managed-by":"platform"}},
            {"id":"c4","clientId":"gentian-edge-acme"},
            {"id":"c5","clientId":"https://files.acme.example/sso","protocol":"saml"},
            {"id":"c6","clientId":"gentian-xwiki"},
            {"id":"c7","clientId":"realm-management"}]}`
	w.archive(realm, "", "")
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.clientsHere(`[{"id":"k1","clientId":"acme-wiki"},{"id":"k7","clientId":"realm-management"}]`)
	w.answer("GET", "acme/group-by-path/staff", 200, `{"id":"g-staff","name":"staff"}`)
	w.answer("PUT", "acme/groups/g-staff", 204, ``)
	w.answer("GET", "acme/clients?clientId=gentian-xwiki", 200, `[]`)
	w.answer("GET", "acme/clients?clientId=acme-tool", 200, `[{"id":"k2","clientId":"acme-tool"}]`)
	w.answer("GET", "acme/clients/k2/roles/user", 200, `{"id":"r-tool","name":"user"}`)
	w.answer("POST", "acme/groups/g-staff/role-mappings/clients/k2", 204, ``)
	w.answer("GET", "acme", 200, `{"realm":"acme","defaultRole":{"name":"default-roles-acme"}}`)

	out, err := w.run(importScript("acme", RealmSource{PlatformClients: []string{"gentian-xwiki", "gentian-synapse"}}))
	if err != nil {
		t.Fatalf("the import failed: %v\n%s\n%s", err, out, w.log())
	}
	imports := w.partialImports()
	if len(imports) != 1 {
		t.Fatalf("want one partial import, of the clients and roles, got %d:\n%s", len(imports), w.log())
	}
	clients := imports[0]
	for _, want := range []string{`"ifResourceExists":"SKIP"`, `"clientId":"acme-wiki"`, `"clientId":"acme-tool"`,
		`"client":{"acme-wiki":[{"name":"user"}],"acme-tool":[{"name":"user"}],"realm-management":[{"name":"realm-admin"}]}`,
		`"realm":[{"name":"auditor","composites":{"client":{"acme-tool":["user"]}}}]`,
		`"post.logout.redirect.uris":"+"`, `"https://wiki.old.example/*"`} {
		if !strings.Contains(clients, want) {
			t.Errorf("the clients' import does not carry %s:\n%s", want, clients)
		}
	}
	for _, not := range []string{"**********", `"secret"`, "registrationAccessToken", "OVERWRITE", `"groups"`,
		"acme-marked", "gentian-edge-acme", "files.acme.example", "gentian-xwiki"} {
		if strings.Contains(clients, not) {
			t.Errorf("the clients' import carries %s:\n%s", not, clients)
		}
	}
	for _, want := range []string{
		"the client acme-marked is made by the platform and is not here",
		"the client gentian-edge-acme is made by the platform",
		"the client https://files.acme.example/sso is made by the platform",
		"the client gentian-xwiki is made by the platform",
		"/staff held the role user at the client gentian-xwiki, which is not here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "the client realm-management is made by the platform") {
		t.Errorf("a client that is here is named as missing:\n%s", out)
	}
	if !strings.Contains(compactJSON(w.log()), `BODY POST acme/groups/g-staff/role-mappings/clients/k2 [{"id":"r-tool","name":"user"}]`) {
		t.Errorf("staff was not given its role at acme-tool:\n%s", w.log())
	}
}

// On another domain, a client the tenant made comes back at this tenant's
// addresses: each host at the tenant's old domain, or the cluster's, is the
// same host at the new one -- in the redirect URIs, the web origins, the
// root, base and admin URLs, the addresses in its attributes (post-logout
// redirects among them, "##"-separated) and a clientId that is an address or
// a host. Its roles, a role's composites and a group's roles at it follow
// the new clientId. A host at any other domain is left as it is, and so is
// everything where no move is known.
func TestARealmImportMovesTheTenantsClientsToItsDomain(t *testing.T) {
	realm := `{"realm":"acme",
 "groups":[{"name":"staff","path":"/staff","clientRoles":{"https://app.acme.old.example/saml":["user"]}}],
 "roles":{"realm":[{"name":"auditor","composites":{"client":{"https://app.acme.old.example/saml":["user"]}}}],
          "client":{"https://app.acme.old.example/saml":[{"name":"user"}],"wiki.acme.old.example":[{"name":"user"}]}},
 "clients":[{"clientId":"https://app.acme.old.example/saml","protocol":"saml",
             "redirectUris":["https://app.acme.old.example/*","https://*.acme.old.example/cb","https://portal.old.example/cb",
                             "https://other.example/x","https://notacme.old.example.net/y","/relative"],
             "webOrigins":["+","https://App.Acme.Old.Example:8443"],
             "rootUrl":"https://app.acme.old.example","baseUrl":"/home","adminUrl":"https://admin.old.example/x",
             "attributes":{"post.logout.redirect.uris":"https://app.acme.old.example/bye##+",
                           "backchannel.logout.url":"https://app.acme.old.example/bcl","pkce.code.challenge.method":"S256"}},
            {"clientId":"wiki.acme.old.example"}]}`
	moved := RealmSource{Domain: "acme.old.example", TargetDomain: "acme.new.example",
		KernelDomain: "old.example", TargetKernelDomain: "new.example"}

	w := newRealmWorld(t)
	w.archive(realm, "", "")
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.clientsHere(`[]`)
	w.answer("GET", "acme/group-by-path/staff", 200, `{"id":"g-staff","name":"staff"}`)
	w.answer("PUT", "acme/groups/g-staff", 204, ``)
	w.answer("GET", "acme/clients?clientId=https%3A%2F%2Fapp.acme.new.example%2Fsaml", 200, `[{"id":"k1"}]`)
	w.answer("GET", "acme/clients/k1/roles/user", 200, `{"id":"r1","name":"user"}`)
	w.answer("POST", "acme/groups/g-staff/role-mappings/clients/k1", 204, ``)
	w.answer("GET", "acme", 200, `{"realm":"acme","defaultRole":{"name":"default-roles-acme"}}`)
	out, err := w.run(importScript("acme", moved))
	if err != nil {
		t.Fatalf("the import failed: %v\n%s\n%s", err, out, w.log())
	}
	imports := w.partialImports()
	if len(imports) != 1 {
		t.Fatalf("want one partial import, got %d:\n%s", len(imports), w.log())
	}
	for _, want := range []string{
		`"clientId":"https://app.acme.new.example/saml"`, `"clientId":"wiki.acme.new.example"`,
		`"redirectUris":["https://app.acme.new.example/*","https://*.acme.new.example/cb","https://portal.new.example/cb","https://other.example/x","https://notacme.old.example.net/y","/relative"]`,
		`"webOrigins":["+","https://app.acme.new.example:8443"]`,
		`"rootUrl":"https://app.acme.new.example"`, `"baseUrl":"/home"`, `"adminUrl":"https://admin.new.example/x"`,
		`"post.logout.redirect.uris":"https://app.acme.new.example/bye##+"`,
		`"backchannel.logout.url":"https://app.acme.new.example/bcl"`, `"pkce.code.challenge.method":"S256"`,
		`"client":{"https://app.acme.new.example/saml":[{"name":"user"}],"wiki.acme.new.example":[{"name":"user"}]}`,
		`"composites":{"client":{"https://app.acme.new.example/saml":["user"]}}`,
	} {
		if !strings.Contains(imports[0], want) {
			t.Errorf("the import does not carry %s:\n%s", want, imports[0])
		}
	}
	if strings.Contains(imports[0], "acme.old.example\"") || strings.Contains(imports[0], "app.acme.old.example") {
		t.Errorf("the import still names the old domain:\n%s", imports[0])
	}
	if !strings.Contains(out, "the client https://app.acme.old.example/saml is made at the addresses of this tenant") {
		t.Errorf("the output does not say the client moves:\n%s", out)
	}
	if !strings.Contains(w.log(), "POST acme/groups/g-staff/role-mappings/clients/k1") {
		t.Errorf("staff was not given its role at the moved client:\n%s", w.log())
	}

	// No move known, or none to make: every address as the bundle has it.
	for _, source := range []RealmSource{{}, {Domain: "acme.old.example"},
		{Domain: "acme.old.example", TargetDomain: "ACME.old.example"}, {Domain: "acme.old.example", TargetDomain: "not a domain"}} {
		if got := source.hostMoves(); got != "[]" {
			t.Errorf("%+v moves %s", source, got)
		}
	}
	if got := moved.hostMoves(); got != `[["acme.old.example","acme.new.example"],["old.example","new.example"]]` {
		t.Errorf("the moves are %s", got)
	}
	w = newRealmWorld(t)
	w.archive(realm, "", "")
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.clientsHere(`[]`)
	w.answer("GET", "acme/group-by-path/staff", 200, `{"id":"g-staff","name":"staff"}`)
	w.answer("PUT", "acme/groups/g-staff", 204, ``)
	w.answer("GET", "acme/clients?clientId=https%3A%2F%2Fapp.acme.old.example%2Fsaml", 200, `[]`)
	w.answer("GET", "acme", 200, `{"realm":"acme","defaultRole":{"name":"default-roles-acme"}}`)
	if out, err := w.run(importScript("acme", RealmSource{})); err != nil {
		t.Fatalf("the import failed: %v\n%s\n%s", err, out, w.log())
	}
	if got := w.partialImports(); len(got) != 1 || strings.Contains(got[0], "new.example") ||
		!strings.Contains(got[0], `"rootUrl":"https://app.acme.old.example"`) {
		t.Errorf("without a move the addresses changed:\n%s", got)
	}
}

// A group that is here keeps its id: the platform's Group resource knows it
// by that id, and Keycloak's OVERWRITE would delete it and make it anew
// under the bundle's, with nobody in it. What OVERWRITE brought back is put
// onto it instead: the tenant's own group gets the bundle's description and
// attributes; one of the platform's keeps its own and gets only those it
// lacks; both get the roles the bundle says they held. A group of the
// platform's that is not here is not made from the bundle -- by its mark, or
// by its name in a bundle without one -- and the tenant's that are not here
// are made: under the bundle's ids at the top, under a new one inside a
// group that is here. People are still put back into their groups by path.
func TestARealmImportKeepsTheIdsOfTheGroupsThatAreHere(t *testing.T) {
	w := newRealmWorld(t)
	realm := `{"realm":"acme",
 "roles":{"realm":[{"name":"auditor"}]},
 "groups":[
  {"id":"b-members","name":"gentian:tenant:acme:members","path":"/gentian:tenant:acme:members",
   "attributes":{"gentianos.io/managed-by":["platform"]}},
  {"id":"b-app","name":"gentian:tenant:acme:app:wiki","path":"/gentian:tenant:acme:app:wiki",
   "attributes":{"gentianos.io/managed-by":["platform"],"gentianDefaultGrant":["true"],"odooRoles":["old"]}},
  {"id":"b-perimeter","name":"gentian:tenant:acme:perimeter","path":"/gentian:tenant:acme:perimeter"},
  {"id":"b-staff","name":"staff","path":"/staff","description":"Everyone","attributes":{"floor":["2"]},
   "realmRoles":["auditor"],
   "subGroups":[{"id":"b-board","name":"board","path":"/staff/board"}]},
  {"id":"b-guests","name":"guests","path":"/guests","realmRoles":["auditor"],
   "subGroups":[{"id":"b-vip","name":"vip","path":"/guests/vip","clientRoles":{"acme-wiki":["user"]}},
                {"id":"b-sneak","name":"gentian:tenant:acme:admins","path":"/guests/gentian:tenant:acme:admins"}]}]}`
	memberships := `{"userId":"u1","groups":[{"path":"/gentian:tenant:acme:app:wiki"},{"path":"/staff/board"},{"path":"/guests/vip"}]}
`
	w.archive(realm, `{"id":"u1","username":"ada"}
`, memberships)
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.clientsHere(`[{"id":"k1","clientId":"acme-wiki"}]`)
	// Here: members, the app's group (with the platform's own odooRoles),
	// staff; not here: perimeter, board, guests and what is in it.
	w.answer("GET", "acme/group-by-path/gentian%3Atenant%3Aacme%3Amembers", 200,
		`{"id":"g-members","name":"gentian:tenant:acme:members","attributes":{"gentianos.io/managed-by":["platform"]}}`)
	w.answer("GET", "acme/group-by-path/gentian%3Atenant%3Aacme%3Aapp%3Awiki", 200,
		`{"id":"g-app","name":"gentian:tenant:acme:app:wiki","attributes":{"gentianos.io/managed-by":["platform"],"odooRoles":["new"]}}`)
	w.answer("PUT", "acme/groups/g-app", 204, ``)
	w.answer("GET", "acme/group-by-path/staff", 200, `{"id":"g-staff","name":"staff","attributes":{"floor":["1"],"extra":["x"]}}`)
	w.answer("PUT", "acme/groups/g-staff", 204, ``)
	w.answer("GET", "acme/roles/auditor", 200, `{"id":"r-aud","name":"auditor"}`)
	w.answer("POST", "acme/groups/g-staff/role-mappings/realm", 204, ``)
	w.answerNth("GET", "acme/group-by-path/staff/board", 1, 404, `{}`)
	w.answer("GET", "acme/group-by-path/staff/board", 200, `{"id":"g-board","name":"board"}`)
	w.answer("POST", "acme/groups/g-staff/children", 201, `{}`)
	w.answerNth("GET", "acme/group-by-path/guests", 1, 404, `{}`)
	w.answer("GET", "acme/group-by-path/guests", 200, `{"id":"b-guests","name":"guests"}`)
	w.answer("POST", "acme/groups/b-guests/role-mappings/realm", 204, ``)
	w.answer("GET", "acme/group-by-path/guests/vip", 200, `{"id":"b-vip","name":"vip"}`)
	w.answer("PUT", "acme/groups/b-vip", 204, ``)
	w.answer("GET", "acme/clients?clientId=acme-wiki", 200, `[{"id":"k1"}]`)
	w.answer("GET", "acme/clients/k1/roles/user", 200, `{"id":"r-user","name":"user"}`)
	w.answer("POST", "acme/groups/b-vip/role-mappings/clients/k1", 204, ``)
	w.answer("GET", "acme", 200, `{"realm":"acme","defaultRole":{"name":"default-roles-acme"}}`)
	w.answer("GET", "acme/users?exact=true&username=ada", 200, `[{"id":"n1"}]`)
	w.answer("PUT", "acme/users/n1/groups/g-app", 204, ``)
	w.answer("PUT", "acme/users/n1/groups/g-board", 204, ``)
	w.answer("PUT", "acme/users/n1/groups/b-vip", 204, ``)

	out, err := w.run(importScript("acme", RealmSource{}))
	if err != nil {
		t.Fatalf("the import failed: %v\n%s\n%s", err, out, w.log())
	}
	log := compactJSON(w.log())
	for _, want := range []string{
		// The app's group keeps its odooRoles and gains the tenant's choice.
		`BODY PUT acme/groups/g-app {"name":"gentian:tenant:acme:app:wiki","description":null,"attributes":{"gentianos.io/managed-by":["platform"],"gentianDefaultGrant":["true"],"odooRoles":["new"]}}`,
		// Staff is as the bundle recorded it, under its id here.
		`BODY PUT acme/groups/g-staff {"name":"staff","description":"Everyone","attributes":{"floor":["2"]}}`,
		`BODY POST acme/groups/g-staff/role-mappings/realm [{"id":"r-aud","name":"auditor"}]`,
		// Board is made inside it.
		`BODY POST acme/groups/g-staff/children {"name":"board","description":null,"attributes":{}}`,
		// Guests is made with what is in it, under the bundle's ids, without
		// the roles (granted after, by id) or the platform's group inside it.
		`"groups":[{"id":"b-guests","name":"guests","path":"/guests","subGroups":[{"id":"b-vip","name":"vip","path":"/guests/vip"}]}]`,
		`BODY POST acme/groups/b-guests/role-mappings/realm [{"id":"r-aud","name":"auditor"}]`,
		`BODY POST acme/groups/b-vip/role-mappings/clients/k1 [{"id":"r-user","name":"user"}]`,
		// People are put back into their groups by path.
		"PUT acme/users/n1/groups/g-app", "PUT acme/users/n1/groups/g-board", "PUT acme/users/n1/groups/b-vip",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %s:\n%s", want, log)
		}
	}
	for _, not := range []string{"OVERWRITE", "DELETE", "PUT acme/groups/g-members", "perimeter\",", `"b-perimeter"`,
		"b-sneak", `"id":"b-staff"`, `"id":"b-app"`, `"id":"b-members"`} {
		if strings.Contains(log, not) {
			t.Errorf("the import did %s:\n%s", not, log)
		}
	}
	for _, want := range []string{
		"the group /gentian:tenant:acme:perimeter is made by the platform and is not here",
		"/staff/board is made inside /staff, which is here, under a new id",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not say %q:\n%s", want, out)
		}
	}
}

// The platform's group of an app the tenant does not have is not here when
// a restore leaves the app out, or a bundle of another tenant brings people
// who had it: the membership is said and left out, and the import succeeds.
func TestARealmImportLeavesOutTheGroupOfAnAppThatIsNotHere(t *testing.T) {
	w := newRealmWorld(t)
	w.archive(bundleRealm, bundleUsers, `{"userId":"u1","groups":[{"path":"/gentian:tenant:acme:members"},{"path":"/gentian:tenant:acme:app:open-webui"}]}
{"userId":"u3","groups":[{"path":"/gentian:tenant:acme:members"}]}
`)
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.clientsHere(`[{"id":"k1","clientId":"acme-wiki"}]`)
	w.groupsHere()
	w.answer("GET", "acme", 200, `{"realm":"acme","defaultRole":{"name":"default-roles-acme"}}`)
	w.answer("GET", "acme/users?exact=true&username=ada", 200, `[{"id":"n1"}]`)
	w.answer("GET", "acme/users?exact=true&username=bob", 200, `[{"id":"n3"}]`)
	w.answer("GET", "acme/group-by-path/gentian%3Atenant%3Aacme%3Amembers", 200, `{"id":"g-members"}`)
	w.answer("PUT", "acme/users/n1/groups/g-members", 204, ``)
	w.answer("PUT", "acme/users/n3/groups/g-members", 204, ``)

	out, err := w.run(importScript("acme", RealmSource{}))
	if err != nil {
		t.Fatalf("the import failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ada was a member of /gentian:tenant:acme:app:open-webui, the group of an app that is not here") {
		t.Errorf("the membership left out is not said:\n%s", out)
	}
}

// What the import used to pass over in silence now fails it, with the name:
// a person the provider refuses, a group that is not there, a membership
// that is refused -- and it still goes to the end, so the output names all.
func TestARealmImportFailsForWhatItCouldNotPutBack(t *testing.T) {
	w := newRealmWorld(t)
	w.archive(bundleRealm, bundleUsers, bundleMemberships)
	w.answer("POST", "acme/partialImport", 200, `{}`)
	w.clientsHere(`[{"id":"k1","clientId":"acme-wiki"}]`)
	w.answer("GET", "acme", 200, `{"realm":"acme","defaultRole":{"name":"default-roles-acme"}}`)
	w.answer("GET", "acme/users?exact=true&username=ada", 200, `[]`)
	// Staff and its board are here; the members group, the platform's, is
	// not, and nothing answers for it. The first partial import is the
	// realm's clients and roles, the second ada.
	w.answer("GET", "acme/group-by-path/staff", 200, `{"id":"g-staff","name":"staff"}`)
	w.answer("GET", "acme/group-by-path/staff/board", 200, `{"id":"g-board","name":"board"}`)
	w.answer("PUT", "acme/groups/g-staff", 204, ``)
	w.answer("PUT", "acme/groups/g-board", 204, ``)
	w.answerNth("POST", "acme/partialImport", 2, 409, `{"errorMessage":"User exists with same email"}`)
	w.answer("GET", "acme/users?exact=true&username=bob", 200, `[{"id":"n3"}]`)

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
	cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "STUB=" + dir,
		"TERMINATION_LOG=" + filepath.Join(dir, "reported")}
	out, err := cmd.CombinedOutput()
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	reported, _ := os.ReadFile(filepath.Join(dir, "reported"))
	return string(out) + "\n--- log\n" + string(log) + "\n--- reported\n" + string(reported), err
}

// One PostgreSQL server holds every tenant's databases, and a database an
// app made for itself is named whatever the app chose. Where the tenant's
// database prefix here is not the one it had where the bundle was taken, each
// lands under the provisioned database name here, and nothing under the old
// prefix is addressed; and whatever the naming does, a database another role
// owns is never replaced.
func TestOwnedDatabasesAreRestoredUnderTheTargetsNamesAndNeverOverAnothers(t *testing.T) {
	// Under another prefix: nothing under the old one is addressed.
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
	// And says what it put back under which name, for the app's commands.
	if _, reported, _ := strings.Cut(out, "--- reported\n"); !slices.Equal(ParseOwnedNames(reported),
		[]string{"acme_wiki_reports=acme2_wiki_reports", "analytics=acme2_wiki_analytics"}) {
		t.Errorf("reported %q", reported)
	}
	for _, not := range []string{"LOAD acme_wiki_reports", "LOAD analytics", "CREATE analytics"} {
		if strings.Contains(out, not) {
			t.Errorf("the original's database was written to (%s):\n%s", not, out)
		}
	}

	// Under the same provisioned name: each under its own name.
	out, err = runOwnedRestore(t, "acme_wiki", "acme_wiki", []string{"analytics"}, map[string]string{"analytics": "acme2_wiki"})
	if err != nil || !strings.Contains(out, "restored analytics as analytics") {
		t.Errorf("a restore under the same prefix renamed a database: %v\n%s", err, out)
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
