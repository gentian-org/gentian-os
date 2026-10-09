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

	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The mailbox Jobs run the mail server's own tool beside the mail server's
// volume, as the user it keeps mail as. Which image, which directory and
// which user that is, is the mail server's chart's to say: a chart that
// moves on without these would leave the Jobs reading a layout that is no
// longer there, with a doveadm of another version.
func TestTheMailboxJobsAreTheMailServersOwn(t *testing.T) {
	chart, err := os.ReadFile(filepath.Join("..", "..", "kernel", "services", "dovecot", "manifests", "templates", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"image: " + DovecotImage,
		"mail_location = " + MailLayout,
		"mail_uid = 1000", "mail_gid = 1000",
		"mountPath: " + MailRoot,
	} {
		if !strings.Contains(string(chart), want) {
			t.Errorf("the mail server's chart does not say %q, which the mailbox Jobs are built on", want)
		}
	}
	if !strings.HasPrefix(MailLayout, "maildir:"+MailRoot+"/%d/%n") {
		t.Errorf("the Jobs take a mailbox to be %s/<domain>/<name>, and the layout is %s", MailRoot, MailLayout)
	}
}

func mailParams() (JobParams, Decryption) {
	p := JobParams{Namespace: "system-mail", Name: "j", Tenant: "demo", App: "gentian-tenant", Export: "nightly", Bucket: "b", Prefix: "p", Node: "node-1",
		Encryption: Encryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, Recipients: []string{"age1qqqsyqcyq5rqwzqfpg9scrgwpugpzysnzs23v9ccrydpk8qarc0sxpzkh"}}}
	return p, Decryption{Mode: gentianov1alpha1.ExportEncryptionRecipient, SecretName: "k", SecretKey: "identity"}
}

// The three Jobs: each beside the volume, on its node, as the mail server's
// user, and under a context the namespace admits.
func TestTheMailboxJobs(t *testing.T) {
	p, d := mailParams()
	backupJob := MailboxBackupJob(p, "dovecot-dev-mail", "demo.example")
	restoreJob := MailboxRestoreJob(p, d, MailboxesArtefact, "dovecot-dev-mail", "new.example")
	destroy := MailboxDestroyJob("system-mail", "demo", "dovecot-dev-mail", "node-1", "demo.example", DestroyInTheBackground)

	cases := map[string]struct {
		pod       corev1.PodSpec
		container string
	}{
		"backup":  {backupJob.Spec.Template.Spec, "mailbox-backup"},
		"restore": {restoreJob.Spec.Template.Spec, "mailbox-restore"},
		"destroy": {destroy.Spec.Template.Spec, "delete-mailboxes"},
	}
	for act, c := range cases {
		var found *corev1.Container
		for _, group := range [][]corev1.Container{c.pod.InitContainers, c.pod.Containers} {
			for i := range group {
				if group[i].Name == c.container {
					found = &group[i]
				}
			}
		}
		if found == nil {
			t.Fatalf("%s: no container %s", act, c.container)
		}
		if found.Image != DovecotImage {
			t.Errorf("%s runs %s, not the mail server's image", act, found.Image)
		}
		sc := found.SecurityContext
		if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != MailUser || sc.RunAsGroup == nil || *sc.RunAsGroup != MailUser {
			t.Errorf("%s does not run as the user the mail server keeps mail as", act)
		}
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.SeccompProfile == nil {
			t.Errorf("%s: the pod would be refused admission", act)
		}
		if c.pod.NodeSelector[corev1.LabelHostname] != "node-1" {
			t.Errorf("%s is not held to the node the mail server has the volume on: %v", act, c.pod.NodeSelector)
		}
		mounted := false
		for _, v := range c.pod.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "dovecot-dev-mail" {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("%s does not mount the mail server's volume", act)
		}
		script := found.Args[0]
		shell := found.Command[0]
		if out, err := exec.Command(shell, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %s -n: %v\n%s", act, shell, err, out)
		}
		if !strings.HasPrefix(script, "set -eu") {
			t.Errorf("%s: the script does not stop at the first failing command", act)
		}
		for _, line := range strings.Split(script, "\n") {
			if strings.Contains(line, "|| true") || strings.Contains(line, "|| echo") || strings.Contains(line, "2>/dev/null") {
				t.Errorf("%s: a failure is discarded or hidden: %s", act, line)
			}
		}
	}

	// A copy goes by the mail server's own synchronisation, never by an
	// archive of a mailbox that is being written to, and is counted before it
	// is believed.
	copyScript := containerByName(backupJob, "mailbox-backup").Args[0]
	for _, want := range []string{`dv "${src}" backup "maildir:${dst}"`, `count "${src}"`, `count "${dst}"`, "inherit_errexit", "INDEX"} {
		if !strings.Contains(copyScript, want) {
			t.Errorf("the backup lacks %q", want)
		}
	}
	if strings.Contains(copyScript, "tar ") {
		t.Error("the backup archives the live mailboxes instead of synchronising them")
	}
	if upload := backupJob.Spec.Template.Spec.Containers[0].Args[0]; !strings.Contains(upload, MailboxesArtefact+EncryptedSuffix) {
		t.Errorf("the backup does not upload to %s", MailboxesArtefact)
	}
	// A restore adds and removes nothing; a name from the bundle is a
	// directory's name or it is refused, and so is a link.
	back := containerByName(restoreJob, "mailbox-restore").Args[0]
	for _, want := range []string{`sync -1 -R "maildir:${src}"`, `.|..|*/*|*[[:cntrl:]]*) echo "ERROR: refused`} {
		if !strings.Contains(back, want) {
			t.Errorf("the restore lacks %q", want)
		}
	}
	if !strings.Contains(containerByName(restoreJob, "unpack-mailboxes").Args[0], "! -type f ! -type d") {
		t.Error("the restore does not refuse a link in the archive")
	}
	if got := containerByName(restoreJob, "mailbox-restore").Env; len(got) != 1 || got[0].Value != "new.example" {
		t.Errorf("the restore writes into %v, not the domain of the tenant restored into", got)
	}

	// The destroy Job is found and bounded like a store's.
	if destroy.Name != "mail-delete-demo" || destroy.Namespace != "system-mail" || destroy.Labels["gentianos.io/tenant"] != "demo" {
		t.Errorf("destroy job %s/%s %v", destroy.Namespace, destroy.Name, destroy.Labels)
	}
	if destroy.Spec.BackoffLimit == nil || *destroy.Spec.BackoffLimit != 1 || destroy.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("a destroy Job that cannot do its work must fail, not retry for ever")
	}
	gone := mailboxDestroyScript()
	for _, want := range []string{`rm -rf -- "${ROOT:?}/${DOMAIN:?}"`, "are still there", `""|.|..|*/*) echo "ERROR: refused`} {
		if !strings.Contains(gone, want) {
			t.Errorf("the destroy script lacks %q", want)
		}
	}
}

// A mail domain's name becomes a directory's: one that would be another
// directory is refused before a Job is built.
func TestAMailDomainIsADirectoryName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "../x", "a b", "-rf", "a\nb"} {
		if ValidMailDomain(bad) == nil {
			t.Errorf("%q is taken for a mail domain", bad)
		}
	}
	for _, good := range []string{"demo.example", "mail.demo.k.example"} {
		if err := ValidMailDomain(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	if !IsPlatformStore(mailStore) {
		t.Error("the name the mailbox Jobs are labelled with can be taken for an app's")
	}
}
