/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package backup

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func recipientDecryption() Decryption {
	return Decryption{
		Mode:       gentianov1alpha1.ExportEncryptionRecipient,
		SecretName: "trs-nightly-key",
		SecretKey:  "identity",
	}
}

func passphraseDecryption() Decryption {
	return Decryption{
		Mode:       gentianov1alpha1.ExportEncryptionPassphrase,
		SecretName: "trs-nightly-key",
		SecretKey:  "passphrase",
	}
}

// A restore cannot open a bundle it has no key for, and the message has to say
// which key is missing — an operator holding the wrong one has no other way to
// find out.
func TestRestoreRefusesWithoutKeyMaterial(t *testing.T) {
	recipient := Decryption{Mode: gentianov1alpha1.ExportEncryptionRecipient}
	err := recipient.Validate()
	if err == nil {
		t.Fatal("accepted a recipient bundle with no identity")
	}
	if !strings.Contains(err.Error(), "identitySecretRef") || !strings.Contains(err.Error(), "off-cluster") {
		t.Errorf("unhelpful message: %v", err)
	}

	passphrase := Decryption{Mode: gentianov1alpha1.ExportEncryptionPassphrase}
	err = passphrase.Validate()
	if err == nil {
		t.Fatal("accepted a passphrase bundle with no passphrase")
	}
	if !strings.Contains(err.Error(), "passphraseSecretRef") {
		t.Errorf("unhelpful message: %v", err)
	}

	if err := recipientDecryption().Validate(); err != nil {
		t.Errorf("valid decryption rejected: %v", err)
	}
}

// The checksum is compared before anything is decrypted or loaded. Restoring a
// truncated dump over a live database is the worst outcome this system has.
func TestRestoreVerifiesChecksumBeforeLoading(t *testing.T) {
	jobs := map[string]*batchv1.Job{
		"postgres": PostgresRestoreJob(params(), recipientDecryption(), "demo_app"),
		"mariadb":  MariaDBRestoreJob(params(), recipientDecryption(), "demo_app"),
		"s3":       S3RestoreJob(params(), recipientDecryption(), "demo-app"),
		"volume":   VolumeRestoreJob(params(), recipientDecryption(), "data"),
		"realm":    RealmImportJob(params(), recipientDecryption(), "demo"),
	}
	for name, job := range jobs {
		fetch := containerByName(job, "fetch")
		if fetch == nil {
			t.Errorf("%s: no fetch step", name)
			continue
		}
		script := strings.Join(fetch.Args, "\n")
		if !strings.Contains(script, "sha256sum") || !strings.Contains(script, "checksum mismatch") {
			t.Errorf("%s: does not verify the artefact before use:\n%s", name, script)
		}
		// And the fetch is an init container, so a mismatch stops the pod
		// before the loading container ever runs.
		var isInit bool
		for _, c := range job.Spec.Template.Spec.InitContainers {
			if c.Name == "fetch" {
				isInit = true
			}
		}
		if !isInit {
			t.Errorf("%s: fetch is not an init container, so a bad artefact would still be loaded", name)
		}
	}
}

func TestRestoreDecryptionUsesTheRightMechanism(t *testing.T) {
	byIdentity := strings.Join(
		containerByName(PostgresRestoreJob(params(), recipientDecryption(), "demo_app"), "fetch").Args, "\n")
	if !strings.Contains(byIdentity, "age -d -i /tmp/identity") {
		t.Errorf("recipient restore does not decrypt with an identity:\n%s", byIdentity)
	}
	if !strings.Contains(byIdentity, "rm -f /tmp/identity") {
		t.Error("the identity is left on disk after use")
	}

	byPassphrase := strings.Join(
		containerByName(PostgresRestoreJob(params(), passphraseDecryption(), "demo_app"), "fetch").Args, "\n")
	if !strings.Contains(byPassphrase, "script -qec") {
		t.Errorf("passphrase restore has no pty; age cannot read a piped passphrase:\n%s", byPassphrase)
	}
}

// Restores replace rather than merge. Anything the bundle does not contain has
// no business surviving a restore that claims to return the app to that point.
func TestDatabaseRestoresReplaceRatherThanMerge(t *testing.T) {
	pg := strings.Join(containerByName(
		PostgresRestoreJob(params(), recipientDecryption(), "demo_app"), "pg-restore").Args, "\n")
	for _, want := range []string{"--clean", "--if-exists", "--single-transaction"} {
		if !strings.Contains(pg, want) {
			t.Errorf("pg_restore missing %s:\n%s", want, pg)
		}
	}

	maria := strings.Join(containerByName(
		MariaDBRestoreJob(params(), recipientDecryption(), "demo_app"), "mariadb-restore").Args, "\n")
	if !strings.Contains(maria, "DROP DATABASE IF EXISTS") || !strings.Contains(maria, "CREATE DATABASE") {
		t.Errorf("mariadb restore does not recreate the database:\n%s", maria)
	}
	// The database name is interpolated into SQL, so the script asserts its
	// shape rather than trusting the caller.
	if !strings.Contains(maria, "grep -qE '^[A-Za-z0-9_]+$'") {
		t.Errorf("mariadb restore does not validate the database name:\n%s", maria)
	}

	s3 := strings.Join(containerByName(
		S3RestoreJob(params(), recipientDecryption(), "demo-app"), "s3-restore").Args, "\n")
	if !strings.Contains(s3, "--remove") {
		t.Errorf("bucket restore merges instead of matching the archive:\n%s", s3)
	}
}

// Volumes are the exception: excludePaths mean the archive is not always a
// complete picture, so wiping the target would turn a documented omission into
// data loss.
func TestVolumeRestoreDoesNotWipeTheTarget(t *testing.T) {
	job := VolumeRestoreJob(params(), recipientDecryption(), "nextcloud-data")
	script := strings.Join(containerByName(job, "volume-restore").Args, "\n")

	if strings.Contains(script, "rm -rf /target") {
		t.Error("volume restore wipes the target, discarding data excludePaths chose not to capture")
	}

	var target *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "target" {
			target = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if target == nil || target.PersistentVolumeClaim == nil {
		t.Fatal("no target claim")
	}
	// Writable here, unlike capture — which is why the controller only runs
	// this with the app already paused.
	if target.PersistentVolumeClaim.ReadOnly {
		t.Error("target claim is read-only; the restore could not write")
	}
}

// Members come back without credentials, and the Job says so rather than
// leaving an operator to learn it from users who cannot sign in.
func TestRealmImportRestoresPeopleAndWarnsAboutPasswords(t *testing.T) {
	script := strings.Join(containerByName(
		RealmImportJob(params(), recipientDecryption(), "demo"), "realm-import").Args, "\n")

	for _, want := range []string{"partialImport", "users.ndjson", "memberships.ndjson", "OVERWRITE"} {
		if !strings.Contains(script, want) {
			t.Errorf("realm import missing %q:\n%s", want, script)
		}
	}
	if !strings.Contains(script, "WITHOUT credentials") {
		t.Error("realm import does not warn that members cannot sign in yet")
	}
}

// Alpine's "mc" package is Midnight Commander. Installing it would satisfy any
// "is mc present" check with a binary that knows nothing about object storage,
// and the failure surfaces as an unrecognisable error on the first alias call.
func TestRestoreDoesNotConfuseMinioClientWithMidnightCommander(t *testing.T) {
	script := strings.Join(
		containerByName(PostgresRestoreJob(params(), recipientDecryption(), "demo_app"), "fetch").Args, "\n")

	if strings.Contains(script, "apk add --no-cache --quiet mc") ||
		strings.Contains(script, "apk add mc") {
		t.Errorf("restore apk-installs 'mc', which is Midnight Commander on Alpine:\n%s", script)
	}
	if !strings.Contains(script, "MCLI=/usr/local/bin/mcli") {
		t.Errorf("restore does not fetch the MinIO client to its own path:\n%s", script)
	}
	// And every call goes through that path, not a bare `mc`.
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "mc ") {
			t.Errorf("bare mc invocation could pick up the wrong binary: %q", trimmed)
		}
	}
}

// A failed restore leaves objects owned by the admin that ran it, and the next
// attempt — running as the app, correctly — cannot drop what it does not own.
// The database wedges precisely when someone is recovering it, and only
// hand-written psql gets it back. So the restore normalises ownership first.
func TestPostgresRestoreNormalisesOwnershipBeforeLoading(t *testing.T) {
	job := PostgresRestoreJob(params(), recipientDecryption(), "demo_nextcloud_base_ce")
	script := job.Spec.Template.Spec.Containers[0].Args[0]

	// Go's fmt leaves %!verb(...) markers behind when a format string escapes
	// its own percent signs wrongly — and SQL is full of them.
	if strings.Contains(script, "%!") {
		t.Fatalf("format verb mishandled in rendered script:\n%s", script)
	}
	// Kubernetes collapses $$ to a single $ in container args — it is the
	// escape for its own $(VAR) syntax — so a script that writes $$ does not
	// arrive as $$. "DO $$" became "DO $" and failed the restore at its first
	// statement. Tagged dollar-quotes ($tag$) use single $ and survive.
	if strings.Contains(script, "$$") {
		t.Errorf("script contains $$, which Kubernetes collapses to $:\n%s", script)
	}
	// psql substitutes :'var' only outside quoted literals, and a dollar-quoted
	// PL/pgSQL body is a quoted literal — the variable reached postgres as a
	// bare colon. Generated statements through \gexec need no procedural block,
	// so neither hazard applies.
	if strings.Contains(script, "DO $") {
		t.Errorf("procedural block reintroduced; psql cannot interpolate inside it:\n%s", script)
	}
	for _, want := range []string{
		"ALTER SCHEMA",
		"OWNER TO",
		`\gexec`,
		"pg_restore --role=",
		// Extension members, and serial/identity sequences, are owned by
		// something else that owns them: skipping them is what lets the rest
		// of the normaliser run to completion.
		"deptype IN ('a', 'i', 'e')",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// Ownership must be settled before the load, or the load is what fails.
	if strings.Index(script, "ALTER SCHEMA") > strings.Index(script, "pg_restore") {
		t.Error("ownership is normalised after the restore, which is too late")
	}
}

// A restore reads from wherever the bundle lives and writes into the cluster's
// own MinIO. Those are one system only when bundles go to platform storage;
// with an external destination they are two, with different credentials.
//
// Sharing one alias sent both at the destination: the restore tried to create
// the app's bucket in someone else's account, which the policy's object-scoped
// credential is denied, and had it been permitted it would have mirrored the
// tenant's objects into the bundle bucket rather than restoring them.
func TestS3RestoreWritesToPlatformStorageNotTheBundleDestination(t *testing.T) {
	p := params()
	p.Endpoint = "https://sos-ch-dk-2.exo.io"
	p.Region = "ch-dk-2"
	p.UploadCredentialsSecret = "backup-destination-corp"

	job := S3RestoreJob(p, recipientDecryption(), "corp-nextcloud")

	var restore *corev1.Container
	for i := range job.Spec.Template.Spec.Containers {
		if job.Spec.Template.Spec.Containers[i].Name == "s3-restore" {
			restore = &job.Spec.Template.Spec.Containers[i]
		}
	}
	if restore == nil {
		t.Fatal("no s3-restore container")
	}

	// Every credential it uses names the platform's own Secret.
	for _, env := range restore.Env {
		if env.Name == "MINIO_ENDPOINT" && env.Value != "" {
			t.Errorf("endpoint is the bundle's literal %q; the app's bucket does not live there", env.Value)
		}
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			if got := env.ValueFrom.SecretKeyRef.Name; got != MinIOAdminSecret {
				t.Errorf("%s comes from %q, want the platform's %q", env.Name, got, MinIOAdminSecret)
			}
		}
	}

	script := restore.Args[0]
	if !strings.Contains(script, `mc mirror --preserve --overwrite --remove /work/restore "platform/corp-nextcloud"`) {
		t.Errorf("restore does not mirror into platform storage:\n%s", script)
	}
	if strings.Contains(script, `"gentian/corp-nextcloud"`) {
		t.Errorf("restore still addresses the bundle alias:\n%s", script)
	}
}

// The portal shell restores into the role the provisioner made, not the one
// its label implies.
//
// This is the one place where a Job's label and its database disagree on
// purpose: the shell is labelled with the tenant-wide component so its progress
// aggregates there, while its database and role are named for the shell. The
// role was derived from the label, so every restore ended at
//
//	ERROR: role "<tenant>_gentian-tenant" does not exist
//
// after the bundle had been fetched and decrypted — far enough to look like it
// was working, and only on the tenant-wide step, which is the last one.
func TestPostgresRestoreUsesTheRoleThatExists(t *testing.T) {
	shell := JobParams{
		Namespace: "platform-kernel",
		Name:      "tx-r-shellr",
		Tenant:    "corp",
		App:       "gentian-tenant", // the label, deliberately
		Role:      PostgresRole("corp", "shell"),
	}
	script := PostgresRestoreJob(shell, Decryption{Mode: "recipient", SecretName: "k", SecretKey: "identity"},
		"corp_shell").Spec.Template.Spec.Containers[0].Args[0]

	if !strings.Contains(script, "'corp_shell'") {
		t.Errorf("restore does not name the shell role:\n%s", script)
	}
	if strings.Contains(script, "corp_gentian-tenant") {
		t.Errorf("restore still derives the role from the label:\n%s", script)
	}

	// An app names one thing, so leaving Role empty must keep working.
	app := JobParams{Namespace: "platform-kernel", Name: "tx-r-pgr", Tenant: "corp", App: "docmost-ce"}
	appScript := PostgresRestoreJob(app, Decryption{Mode: "recipient", SecretName: "k", SecretKey: "identity"},
		"corp_docmost_ce").Spec.Template.Spec.Containers[0].Args[0]
	if !strings.Contains(appScript, "'corp_docmost-ce'") {
		t.Errorf("an app's role is no longer derived from its name:\n%s", appScript)
	}
}
