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

func recipientEncryption() Encryption {
	return Encryption{
		Mode:       gentianov1alpha1.ExportEncryptionRecipient,
		Recipients: []string{"age1platformkeyxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
	}
}

func passphraseEncryption() Encryption {
	return Encryption{
		Mode:             gentianov1alpha1.ExportEncryptionPassphrase,
		PassphraseSecret: "tx-nightly-passphrase",
		PassphraseKey:    "passphrase",
	}
}

func encryptedParams(e Encryption) JobParams {
	p := params()
	p.Encryption = e
	return p
}

// There is no unencrypted path. An export that cannot be protected must refuse
// to run rather than write a tenant's data in the clear.
func TestEncryptionRefusesWhenItCannotProtect(t *testing.T) {
	cases := map[string]Encryption{
		"recipient mode with no recipients": {Mode: gentianov1alpha1.ExportEncryptionRecipient},
		"passphrase mode with no secret":    {Mode: gentianov1alpha1.ExportEncryptionPassphrase},
		"unknown mode":                      {Mode: "none"},
		"recipient that is not an age key": {
			Mode:       gentianov1alpha1.ExportEncryptionRecipient,
			Recipients: []string{"ssh-rsa AAAAB3..."},
		},
	}
	for name, enc := range cases {
		if err := enc.Validate(); err == nil {
			t.Errorf("%s: Validate accepted an unprotectable export", name)
		}
	}

	if err := recipientEncryption().Validate(); err != nil {
		t.Errorf("valid recipient encryption rejected: %v", err)
	}
	if err := passphraseEncryption().Validate(); err != nil {
		t.Errorf("valid passphrase encryption rejected: %v", err)
	}
}

// Whether the platform can still read a bundle is a fact an operator has to be
// able to trust, so it is computed from the keys actually used.
func TestPlatformReadableIsHonest(t *testing.T) {
	cluster := []string{"age1platformkeyxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}

	if !recipientEncryption().PlatformReadable(cluster) {
		t.Error("a bundle encrypted to the cluster key should be platform-readable")
	}
	if passphraseEncryption().PlatformReadable(cluster) {
		t.Error("a passphrase bundle must never be reported as platform-readable")
	}

	// An admin naming their own key replaces the cluster's, so the platform
	// loses access — and must say so.
	own := Encryption{
		Mode:       gentianov1alpha1.ExportEncryptionRecipient,
		Recipients: []string{"age1adminownkeyxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
	}
	if own.PlatformReadable(cluster) {
		t.Error("a bundle encrypted only to an admin key is not platform-readable")
	}
}

func initNames(job *batchv1.Job) []string {
	var names []string
	for _, c := range job.Spec.Template.Spec.InitContainers {
		names = append(names, c.Name)
	}
	return names
}

func containerByName(job *batchv1.Job, name string) *corev1.Container {
	for i := range job.Spec.Template.Spec.InitContainers {
		if job.Spec.Template.Spec.InitContainers[i].Name == name {
			return &job.Spec.Template.Spec.InitContainers[i]
		}
	}
	for i := range job.Spec.Template.Spec.Containers {
		if job.Spec.Template.Spec.Containers[i].Name == name {
			return &job.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

// The ordering is the security property: produce, encrypt, then upload. Any
// other order leaves a window where plaintext is uploadable.
func TestEncryptionRunsBeforeUploadForEveryArtefact(t *testing.T) {
	p := encryptedParams(recipientEncryption())
	manifest, err := ManifestJob(p, &Manifest{Tenant: "demo"}, NewBundleInfo("demo", "nightly", "now", p.Encryption))
	if err != nil {
		t.Fatalf("ManifestJob: %v", err)
	}

	jobs := map[string]*batchv1.Job{
		"postgres": PostgresDumpJob(p, "demo_app"),
		"mariadb":  MariaDBDumpJob(p, "demo_app"),
		"volume":   VolumeArchiveJob(p, "data", nil),
		"s3":       S3ArchiveJob(p, "demo-app"),
		"realm":    RealmExportJob(p, "demo"),
		"manifest": manifest,
	}

	for name, job := range jobs {
		names := initNames(job)
		if len(names) < 2 {
			t.Errorf("%s: expected a producer and an encrypt step, got %v", name, names)
			continue
		}
		if names[len(names)-1] != "encrypt" {
			t.Errorf("%s: encrypt is not the last step before upload: %v", name, names)
		}

		upload := containerByName(job, "upload")
		if upload == nil {
			t.Errorf("%s: no upload container", name)
			continue
		}
		script := strings.Join(upload.Args, "\n")
		if !strings.Contains(script, EncryptedSuffix+`"`) {
			t.Errorf("%s: upload does not read the encrypted file:\n%s", name, script)
		}
	}
}

// The plaintext must not survive the encryption step, or the uploader that runs
// next has a readable copy to find.
func TestEncryptionRemovesThePlaintext(t *testing.T) {
	job := PostgresDumpJob(encryptedParams(recipientEncryption()), "demo_app")
	encrypt := containerByName(job, "encrypt")
	if encrypt == nil {
		t.Fatal("no encrypt container")
	}
	script := strings.Join(encrypt.Args, "\n")

	if !strings.Contains(script, "rm -f '/work/dump.pgc'") {
		t.Errorf("plaintext is not removed:\n%s", script)
	}
	// And the step must fail loudly if it produced nothing, rather than let an
	// empty artefact upload as a successful backup.
	if !strings.Contains(script, "encryption produced no output") {
		t.Errorf("no empty-output guard:\n%s", script)
	}
}

func TestRecipientModeEncryptsToEveryRecipient(t *testing.T) {
	enc := Encryption{
		Mode: gentianov1alpha1.ExportEncryptionRecipient,
		Recipients: []string{
			"age1platformkeyxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			"age1secondkeyxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		},
	}
	script := strings.Join(containerByName(
		PostgresDumpJob(encryptedParams(enc), "demo_app"), "encrypt").Args, "\n")

	for _, r := range enc.Recipients {
		if !strings.Contains(script, "-r '"+r+"'") {
			t.Errorf("recipient %s missing:\n%s", r, script)
		}
	}
	// No passphrase machinery leaks into recipient mode.
	if strings.Contains(script, "script -qec") || strings.Contains(script, PassphraseEnvVar) {
		t.Errorf("recipient mode pulled in passphrase handling:\n%s", script)
	}
}

// age reads a passphrase from a terminal and refuses a pipe, so unattended use
// needs a pty. This pins that the workaround is present and that the passphrase
// arrives from a Secret rather than an argument, where it would be visible in
// the pod spec to anyone who can read it.
func TestPassphraseModeUsesAPtyAndReadsFromASecret(t *testing.T) {
	encrypt := containerByName(
		PostgresDumpJob(encryptedParams(passphraseEncryption()), "demo_app"), "encrypt")
	script := strings.Join(encrypt.Args, "\n")

	if !strings.Contains(script, "script -qec") {
		t.Errorf("no pty wrapper; age -p cannot read a piped passphrase:\n%s", script)
	}
	if !strings.Contains(script, "age -p -o") {
		t.Errorf("passphrase mode does not use age -p:\n%s", script)
	}
	if !strings.Contains(script, "util-linux") {
		t.Errorf("script(1) is not installed:\n%s", script)
	}
	if !strings.Contains(script, "passphrase is empty") {
		t.Errorf("no guard against an empty passphrase:\n%s", script)
	}

	var fromSecret bool
	for _, env := range encrypt.Env {
		if env.Name != PassphraseEnvVar {
			continue
		}
		if env.Value != "" {
			t.Error("passphrase is a literal in the pod spec")
		}
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			fromSecret = true
			if env.ValueFrom.SecretKeyRef.Name != "tx-nightly-passphrase" {
				t.Errorf("passphrase Secret = %q", env.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	if !fromSecret {
		t.Error("passphrase does not come from a Secret")
	}
}

// The bundle header is the one unencrypted file, and it must stay a header:
// enough to identify and open the bundle, nothing about its contents.
func TestBundleInfoTellsYouHowToOpenItAndNothingMore(t *testing.T) {
	byRecipient := NewBundleInfo("demo", "nightly", "2026-08-18T03:00:00Z", recipientEncryption())
	if byRecipient.Recipients == nil {
		t.Error("recipient bundle does not say which key opens it")
	}
	if !strings.Contains(byRecipient.HowToDecrypt, "age -d -i") {
		t.Errorf("unhelpful instructions: %q", byRecipient.HowToDecrypt)
	}

	byPassphrase := NewBundleInfo("demo", "nightly", "2026-08-18T03:00:00Z", passphraseEncryption())
	if byPassphrase.Recipients != nil {
		t.Error("passphrase bundle should list no recipients")
	}
	if !strings.Contains(byPassphrase.HowToDecrypt, "age -d") {
		t.Errorf("unhelpful instructions: %q", byPassphrase.HowToDecrypt)
	}
	if strings.Contains(byPassphrase.HowToDecrypt, "-i ") {
		t.Error("passphrase instructions mention an identity file")
	}
}

// Bucket contents are the bulk of most tenants' data. Mirroring them object by
// object would have put them in the bundle as plaintext.
func TestBucketContentsAreEncryptedLikeEverythingElse(t *testing.T) {
	job := S3ArchiveJob(encryptedParams(recipientEncryption()), "demo-nextcloud")

	if containerByName(job, "encrypt") == nil {
		t.Fatal("bucket capture has no encryption step")
	}
	upload := strings.Join(containerByName(job, "upload").Args, "\n")
	if !strings.Contains(upload, "s3/demo-nextcloud.tar.gz"+EncryptedSuffix) {
		t.Errorf("bucket artefact is not the encrypted archive:\n%s", upload)
	}
}
