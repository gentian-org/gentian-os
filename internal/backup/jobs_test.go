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
	"github.com/gentian-org/gentian-os/internal/meta"
)

func params() JobParams {
	return JobParams{
		Namespace:    "platform-kernel",
		Name:         "tx-demo-app-pg",
		Tenant:       "demo",
		App:          "nextcloud-base-ce",
		Export:       "nightly",
		Bucket:       "demo-gentian-backup",
		Prefix:       "nightly",
		ScratchLimit: "20Gi",
		BackoffLimit: 2,
		Encryption: Encryption{
			Mode:       gentianov1alpha1.ExportEncryptionRecipient,
			Recipients: []string{"age1testkeyxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		},
	}
}

func initContainerScript(job *batchv1.Job) string {
	if len(job.Spec.Template.Spec.InitContainers) == 0 {
		return ""
	}
	return strings.Join(job.Spec.Template.Spec.InitContainers[0].Args, "\n")
}

func mainScript(job *batchv1.Job) string {
	if len(job.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	return strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
}

// The producer writes a fixed filename and the artefact is named after the
// database. Deriving one from the other uploads nothing at all, silently, so
// the two are pinned together here.
func TestPostgresDumpUploadsTheFileItActuallyWrote(t *testing.T) {
	job := PostgresDumpJob(params(), "demo_nextcloud_base_ce")

	dump := initContainerScript(job)
	if !strings.Contains(dump, "--file=/work/dump.pgc") {
		t.Fatalf("dump does not write the expected path:\n%s", dump)
	}

	// The upload reads the encrypted form of exactly that file: the encrypt
	// step renames it, and losing that correspondence uploads nothing.
	upload := mainScript(job)
	if !strings.Contains(upload, `mc cp "/work/dump.pgc`+EncryptedSuffix+`"`) {
		t.Errorf("upload does not read the encrypted file the dump wrote:\n%s", upload)
	}
	if !strings.Contains(upload, "demo-gentian-backup/nightly/postgres/demo_nextcloud_base_ce.pgc") {
		t.Errorf("upload target is wrong:\n%s", upload)
	}
	if !strings.Contains(upload, "sha256sum") {
		t.Error("upload records no checksum")
	}
}

// A dump that fails must never be followed by an upload, or the bundle gains a
// truncated artefact and the export reports success. Init containers give that
// for free: they run in order and a failure stops the pod.
func TestProducerAndEncryptRunAsInitContainersBeforeUpload(t *testing.T) {
	for name, job := range map[string]*batchv1.Job{
		"postgres": PostgresDumpJob(params(), "demo_app"),
		"mariadb":  MariaDBDumpJob(params(), "demo_app"),
		"volume":   VolumeArchiveJob(params(), "data", nil),
		"s3":       S3ArchiveJob(params(), "demo-app"),
		"realm":    RealmExportJob(params(), "demo"),
	} {
		inits := job.Spec.Template.Spec.InitContainers
		if len(inits) != 2 {
			t.Errorf("%s: want a producer and an encrypt step, got %d", name, len(inits))
			continue
		}
		if inits[1].Name != "encrypt" {
			t.Errorf("%s: encrypt is not the final init step: %s", name, inits[1].Name)
		}
		if len(job.Spec.Template.Spec.Containers) != 1 {
			t.Errorf("%s: want exactly one main container, got %d",
				name, len(job.Spec.Template.Spec.Containers))
		}
	}
}

func TestMariaDBDumpUploadsTheFileItWrote(t *testing.T) {
	job := MariaDBDumpJob(params(), "demo_app")
	if !strings.Contains(initContainerScript(job), "> /work/dump.sql.gz") {
		t.Errorf("dump path unexpected:\n%s", initContainerScript(job))
	}
	if !strings.Contains(mainScript(job), `mc cp "/work/dump.sql.gz`+EncryptedSuffix+`"`) {
		t.Errorf("upload path unexpected:\n%s", mainScript(job))
	}
}

func TestVolumeArchiveMountsClaimReadOnlyAndAppliesExcludes(t *testing.T) {
	job := VolumeArchiveJob(params(), "nextcloud-data", []string{"**/appdata_*/preview", "**/cache"})

	var source *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "source" {
			source = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if source == nil || source.PersistentVolumeClaim == nil {
		t.Fatal("no source claim volume")
	}
	if source.PersistentVolumeClaim.ClaimName != "nextcloud-data" {
		t.Errorf("claim = %q", source.PersistentVolumeClaim.ClaimName)
	}
	// Read-only matters: a capture must never be able to modify the data it is
	// preserving, even through a bug in the archive command.
	if !source.PersistentVolumeClaim.ReadOnly {
		t.Error("source claim is not mounted read-only")
	}

	script := initContainerScript(job)
	for _, want := range []string{`--exclude='**/appdata_*/preview'`, `--exclude='**/cache'`} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %s in:\n%s", want, script)
		}
	}
}

// Bucket capture stages on disk rather than mirroring object to object, and
// that is a deliberate reversal: mirroring needed no scratch space but put the
// objects into the bundle as plaintext, since artefacts are encrypted
// individually. Paying for staging is what makes bucket contents as protected
// as everything else.
func TestS3CaptureStagesSoItCanBeEncrypted(t *testing.T) {
	job := S3ArchiveJob(params(), "demo-nextcloud-base-ce")

	var staged bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "work" {
			staged = true
		}
	}
	if !staged {
		t.Error("bucket capture has no scratch volume, so it cannot encrypt before upload")
	}

	fetch := initContainerScript(job)
	if !strings.Contains(fetch, "mc mirror --preserve") {
		t.Errorf("bucket is not fetched with metadata preserved:\n%s", fetch)
	}
	if !strings.Contains(fetch, "tar czf") {
		t.Errorf("bucket is not archived before encryption:\n%s", fetch)
	}
}

// Keycloak's partial-export carries no users, so a realm capture that only
// called it would restore an empty workspace.
func TestRealmExportCapturesUsersAndMemberships(t *testing.T) {
	script := initContainerScript(RealmExportJob(params(), "demo"))

	for _, want := range []string{"partial-export", "/users?", "/groups", "users.ndjson", "memberships.ndjson"} {
		if !strings.Contains(script, want) {
			t.Errorf("realm export missing %q:\n%s", want, script)
		}
	}
	// Paging: a realm bigger than one page would otherwise be captured
	// half-empty with nothing reporting an error.
	if !strings.Contains(script, "first=$((first + page_size))") {
		t.Error("user fetch does not page")
	}
}

func TestCaptureJobsAreBoundedAndLabelled(t *testing.T) {
	job := PostgresDumpJob(params(), "demo_app")

	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 2 {
		t.Error("capture Job has no bounded backoff")
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Errorf("restart policy = %q", job.Spec.Template.Spec.RestartPolicy)
	}
	if job.Labels[ExportLabel] != "nightly" {
		t.Errorf("export label = %q", job.Labels[ExportLabel])
	}
	if job.Labels["gentianos.io/tenant"] != "demo" {
		t.Errorf("tenant label = %q", job.Labels["gentianos.io/tenant"])
	}

	// The scratch mount must be bounded, or one oversized tenant fills a node's
	// ephemeral storage and evicts unrelated workloads.
	var work *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "work" {
			work = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if work == nil || work.EmptyDir == nil || work.EmptyDir.SizeLimit == nil {
		t.Fatal("scratch volume is unbounded")
	}
	if got := work.EmptyDir.SizeLimit.String(); got != "20Gi" {
		t.Errorf("scratch limit = %s", got)
	}
}

func TestCapturesNeverMountTheMasterPassword(t *testing.T) {
	jobs := []*batchv1.Job{
		PostgresDumpJob(params(), "demo_app"),
		MariaDBDumpJob(params(), "demo_app"),
		S3ArchiveJob(params(), "demo-app"),
		VolumeArchiveJob(params(), "data", nil),
		RealmExportJob(params(), "demo"),
	}
	allowed := map[string]struct{}{
		MinIOAdminSecret:    {},
		PostgresAdminSecret: {},
		MariaDBAdminSecret:  {},
		KeycloakAdminSecret: {},
	}
	for _, job := range jobs {
		containers := append([]corev1.Container{}, job.Spec.Template.Spec.InitContainers...)
		containers = append(containers, job.Spec.Template.Spec.Containers...)
		for _, c := range containers {
			for _, env := range c.Env {
				if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
					continue
				}
				name := env.ValueFrom.SecretKeyRef.Name
				if _, ok := allowed[name]; !ok {
					t.Errorf("%s reads unexpected secret %q", job.Name, name)
				}
			}
			for _, env := range c.Env {
				if env.Value != "" && strings.Contains(strings.ToLower(env.Name), "master") {
					t.Errorf("%s carries a master-password-shaped env var %q", job.Name, env.Name)
				}
			}
		}
	}
}

func TestManifestJobEmbedsCompactJSON(t *testing.T) {
	job, err := ManifestJob(params(), &Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Tenant:        "demo",
		Export:        "nightly",
		// A display name with a quote and a newline is the shape that breaks
		// naive here-doc embedding.
		Apps: []ManifestApp{{Name: "app\"one", ChartVersion: "1.0.0"}},
	}, NewBundleInfo("demo", "nightly", "now", params().Encryption))
	if err != nil {
		t.Fatalf("ManifestJob: %v", err)
	}
	// The manifest is staged, then encrypted, then uploaded like any other
	// artefact — it carries the tenant's spec and app inventory, which has no
	// business sitting readable beside an encrypted bundle.
	staged := initContainerScript(job)
	if strings.Count(staged, "GENTIAN_MANIFEST_EOF") != 2 {
		t.Errorf("heredoc delimiters unbalanced:\n%s", staged)
	}
	if !strings.Contains(staged, "> /work/manifest.json") {
		t.Errorf("manifest is not staged for encryption:\n%s", staged)
	}
	// Compact JSON is what keeps a display name from ever producing a line that
	// matches the delimiter.
	if strings.Contains(staged, "\n  \"tenant\"") {
		t.Error("manifest JSON is indented; it must be compact")
	}

	upload := strings.Join(containerByName(job, "upload").Args, "\n")
	if !strings.Contains(upload, "nightly/manifest.json"+EncryptedSuffix) {
		t.Errorf("manifest is not uploaded encrypted:\n%s", upload)
	}

	// bundle-info.json is the deliberate exception, and goes up in the clear.
	info := containerByName(job, "bundle-info")
	if info == nil {
		t.Fatal("no bundle-info container")
	}
	infoScript := strings.Join(info.Args, "\n")
	if !strings.Contains(infoScript, `mc pipe "gentian/demo-gentian-backup/nightly/bundle-info.json"`) {
		t.Errorf("bundle info target unexpected:\n%s", infoScript)
	}
	if strings.Contains(infoScript, "bundle-info.json"+EncryptedSuffix) {
		t.Error("bundle info must stay readable; it is how you learn to open the rest")
	}
	if !strings.Contains(infoScript, "howToDecrypt") {
		t.Errorf("bundle info does not say how to open the bundle:\n%s", infoScript)
	}
}

// Nothing gates a tenant's readiness on the bundle bucket — coupling every
// install to backup infrastructure would be the wrong trade — so each writer
// creates it. Every one of them, because any can be the first to run.
func TestBundleWritersCreateTheirOwnBucket(t *testing.T) {
	manifest, err := ManifestJob(params(), &Manifest{Tenant: "demo"},
		NewBundleInfo("demo", "nightly", "now", params().Encryption))
	if err != nil {
		t.Fatalf("ManifestJob: %v", err)
	}
	writers := map[string]*batchv1.Job{
		"postgres upload": PostgresDumpJob(params(), "demo_app"),
		"mariadb upload":  MariaDBDumpJob(params(), "demo_app"),
		"volume upload":   VolumeArchiveJob(params(), "data", nil),
		"realm upload":    RealmExportJob(params(), "demo"),
		"s3 mirror":       S3ArchiveJob(params(), "demo-app"),
		"manifest":        manifest,
	}

	for name, job := range writers {
		script := mainScript(job)
		if !strings.Contains(script, `mc mb --ignore-existing "gentian/${BUNDLE_BUCKET}"`) {
			t.Errorf("%s does not ensure the bundle bucket:\n%s", name, script)
		}
		if !strings.Contains(script, `mc anonymous set none "gentian/${BUNDLE_BUCKET}"`) {
			t.Errorf("%s leaves the bundle bucket's anonymous policy untouched", name)
		}

		var bucket string
		for _, env := range job.Spec.Template.Spec.Containers[0].Env {
			if env.Name == "BUNDLE_BUCKET" {
				bucket = env.Value
			}
		}
		if bucket != "demo-gentian-backup" {
			t.Errorf("%s: BUNDLE_BUCKET = %q", name, bucket)
		}
	}
}

func TestShellSingleQuoteNeutralisesQuotes(t *testing.T) {
	if got, want := shellSingleQuote("demo_app"), "'demo_app'"; got != want {
		t.Errorf("got %s want %s", got, want)
	}
	// The POSIX idiom: close the quote, emit an escaped quote, reopen. The
	// result still *contains* the dangerous-looking text, which is the point —
	// it is one literal word, not a quote followed by a command.
	if got, want := shellSingleQuote("a'; rm -rf /"), `'a'\''; rm -rf /'`; got != want {
		t.Errorf("got %s want %s", got, want)
	}
	// Every value must come back as a balanced quoted word.
	for _, v := range []string{"", "plain", "with space", "with'quote", `back\slash`} {
		got := shellSingleQuote(v)
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Errorf("shellSingleQuote(%q) = %s, not a quoted word", v, got)
		}
	}
}

// A shellSingleQuote'd value embedded inside double quotes stops being
// quoting and becomes data: --dbname="'demo_app'" asks postgres for a
// database literally named 'demo_app', quotes included. That exact mistake
// shipped and made every capture fail on its first live run, so every script
// any job renders is scanned for the two-character sequence «"'» — which is
// only ever produced by wrapping an already-quoted value in double quotes.
func TestNoScriptWrapsAQuotedValueInDoubleQuotes(t *testing.T) {
	p := params()
	jobs := map[string]*batchv1.Job{
		"postgres-dump": PostgresDumpJob(p, "demo_app"),
		"mariadb-dump":  MariaDBDumpJob(p, "demo_app"),
		"volume":        VolumeArchiveJob(p, "data", []string{"**/cache"}),
		"s3":            S3ArchiveJob(p, "demo-bucket"),
		"realm-export":  RealmExportJob(p, "demo"),
		"pg-restore":    PostgresRestoreJob(p, recipientDecryption(), "demo_app"),
		"maria-restore": MariaDBRestoreJob(p, recipientDecryption(), "demo_app"),
		"s3-restore":    S3RestoreJob(p, recipientDecryption(), "demo-bucket"),
		"realm-import":  RealmImportJob(p, recipientDecryption(), "demo"),
		"bundle-delete": BundleDeleteJob(p),
	}
	for name, job := range jobs {
		containers := append(append([]corev1.Container{},
			job.Spec.Template.Spec.InitContainers...),
			job.Spec.Template.Spec.Containers...)
		for _, c := range containers {
			for _, arg := range c.Args {
				if strings.Contains(arg, `"'`) {
					t.Errorf("%s/%s: script wraps a single-quoted value in double quotes:\n%s",
						name, c.Name, arg)
				}
			}
		}
	}
}

// Deleting a bundle must only ever be able to delete THAT bundle. The two
// properties that make rm --recursive safe to automate: the prefix is part of
// the target (never the bare bucket), and an empty prefix refuses instead of
// widening the glob to everything the tenant has.
func TestBundleDeleteScopesToThePrefixAndRefusesAnEmptyOne(t *testing.T) {
	job := BundleDeleteJob(params())
	script := job.Spec.Template.Spec.Containers[0].Args[0]

	if !strings.Contains(script, `[ -n "${BUNDLE_PREFIX}" ]`) {
		t.Error("no empty-prefix guard: an export with no prefix would rm the whole bucket")
	}
	if !strings.Contains(script, `"gentian/${BUNDLE_BUCKET}/${BUNDLE_PREFIX}/"`) {
		t.Error("rm target is not scoped to the bundle prefix")
	}
	if strings.Contains(script, `rm --recursive --force "gentian/${BUNDLE_BUCKET}"`) {
		t.Error("script can address the bare bucket")
	}
	// A bundle that was never written (capture failed before first upload)
	// must delete cleanly, or failed exports become undeletable.
	if !strings.Contains(script, "nothing to delete") {
		t.Error("missing absent-bundle tolerance")
	}
	var prefixed bool
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "BUNDLE_PREFIX" && env.Value == params().Prefix {
			prefixed = true
		}
	}
	if !prefixed {
		t.Error("BUNDLE_PREFIX env not wired from JobParams.Prefix")
	}
}

// Volume Jobs run in the tenant namespace, so their credentials cannot come
// from minio-admin — the controller stages a copy and names it in JobParams.
// Every MinIO env var must follow that name, or the Job mounts a Secret that
// does not exist where it runs and sits in CreateContainerConfigError.
func TestVolumeArchiveUsesStagedCredentialsWhenNamed(t *testing.T) {
	p := params()
	p.Namespace = "tenant-demo"
	p.UploadCredentialsSecret = "tx-nightly-vol-creds"
	job := VolumeArchiveJob(p, "nextcloud-data", nil)

	if job.Namespace != "tenant-demo" {
		t.Fatalf("job namespace = %q", job.Namespace)
	}
	containers := append(append([]corev1.Container{},
		job.Spec.Template.Spec.InitContainers...),
		job.Spec.Template.Spec.Containers...)
	for _, c := range containers {
		for _, env := range c.Env {
			ref := env.ValueFrom
			if ref == nil || ref.SecretKeyRef == nil {
				continue
			}
			if strings.HasPrefix(env.Name, "MINIO_") && ref.SecretKeyRef.Name != "tx-nightly-vol-creds" {
				t.Errorf("%s/%s reads %s from %q, not the staged copy",
					c.Name, env.Name, env.Name, ref.SecretKeyRef.Name)
			}
		}
	}
	if got := job.Spec.Template.Labels[meta.ComponentLabel]; got != "tenant-export" {
		t.Errorf("pod component label = %q; the export NetworkPolicy selects on it", got)
	}
}
