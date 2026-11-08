/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/gentian-org/gentian-os/api/bundle"
)

func personTarget() MailboxTarget {
	return MailboxTarget{Namespace: "system-mail", Claim: "dovecot-dev-mail", Node: "node-1", Tenant: "demo", Record: "demo-abc12",
		Domain: "demo.example", Mailbox: "alice", Archive: ArchiveName("alice", time.Date(2026, 10, 9, 14, 30, 0, 0, time.UTC))}
}

// The three Jobs for one person's mailbox are the tenant's mailbox Jobs in
// everything but what they work on: the mail server's image, its user, its
// volume, its node, a context the namespace admits, a script that stops at
// the first failure, hides none, and fails rather than retrying for ever.
func TestTheJobsForOnePersonsMailbox(t *testing.T) {
	target := personTarget()
	if target.Archive != "alice-20261009T143000Z" {
		t.Fatalf("an archive is named %q", target.Archive)
	}
	jobs := map[string]*batchv1.Job{}
	var err error
	if jobs["archive"], err = MailboxArchiveJob(target); err != nil {
		t.Fatal(err)
	}
	if jobs["delete"], err = MailboxDeleteJob(target); err != nil {
		t.Fatal(err)
	}
	if jobs["delete the archive"], err = ArchivedMailboxDeleteJob(target); err != nil {
		t.Fatal(err)
	}
	// One Job per thing done, so that the Job that archived a mailbox is not
	// taken, weeks later, for the one that deletes the archive; and a name a
	// Job can have whatever the record is called.
	if jobs["archive"].Name == jobs["delete the archive"].Name || jobs["archive"].Name == jobs["delete"].Name {
		t.Error("two different Jobs for one record share a name")
	}
	if got := MailboxJobName(strings.Repeat("t", 63)+"-abcde", MailboxActDeleteArchive); len(got) > 63 {
		t.Errorf("a Job would be named %s", got)
	}
	for act, job := range jobs {
		if !strings.HasPrefix(job.Name, "mailbox-") || len(job.Name) > 63 || job.Namespace != "system-mail" || job.Labels["gentianos.io/tenant"] != "demo" || job.Annotations[MailboxRecordAnnotation] != "demo-abc12" {
			t.Errorf("%s: job %s/%s %v %v", act, job.Namespace, job.Name, job.Labels, job.Annotations)
		}
		if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 1 || job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("%s: a Job that cannot do its work must fail, not retry for ever", act)
		}
		pod := job.Spec.Template.Spec
		c := pod.Containers[0]
		if c.Image != DovecotImage {
			t.Errorf("%s runs %s, not the mail server's image", act, c.Image)
		}
		sc := c.SecurityContext
		if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != MailUser || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot ||
			sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.SeccompProfile == nil {
			t.Errorf("%s: not the mail server's user under a context the namespace admits: %+v", act, sc)
		}
		if pod.NodeSelector[corev1.LabelHostname] != "node-1" {
			t.Errorf("%s is not held to the node the mail server has the volume on", act)
		}
		if len(pod.Volumes) != 1 || pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "dovecot-dev-mail" {
			t.Errorf("%s does not mount the mail server's volume and nothing else: %+v", act, pod.Volumes)
		}
		script := c.Args[0]
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v\n%s", act, err, out)
		}
		if !strings.HasPrefix(script, "set -eu") {
			t.Errorf("%s: the script does not stop at the first failing command", act)
		}
		for _, line := range strings.Split(script, "\n") {
			if strings.Contains(line, "|| true") || strings.Contains(line, "|| echo") || strings.Contains(line, "2>/dev/null") {
				t.Errorf("%s: a failure is discarded or hidden: %s", act, line)
			}
		}
		// The names are checked by the script too, and no link is followed.
		for _, want := range []string{`refuse "${DOMAIN} is not a mail domain's directory"`, `no_link "${ROOT}/${DOMAIN}"`, `no_link "${ARCHIVE}"`, "result "} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: the script lacks %q", act, want)
			}
		}
	}
	// An archive is a move on the volume, never a copy that is then deleted;
	// a delete is gone by the volume's listing before it says so.
	move := jobs["archive"].Spec.Template.Spec.Containers[0].Args[0]
	if !strings.Contains(move, `mv -- "${live}" "${kept}"`) || strings.Contains(move, "rm -rf") {
		t.Errorf("the archive script does not move the mailbox, or removes something:\n%s", move)
	}
	for _, act := range []string{"delete", "delete the archive"} {
		script := jobs[act].Spec.Template.Spec.Containers[0].Args[0]
		if !strings.Contains(script, "rm -rf -- ") || !strings.Contains(script, "is still there") {
			t.Errorf("%s: the script does not remove and then look:\n%s", act, script)
		}
	}
	if got := jobs["delete"].Spec.Template.Spec.Containers[0].Args[0]; strings.Contains(got, `"${ARCHIVE:?}`) {
		t.Error("deleting a live mailbox removes something below the archive")
	}
}

// A name that is not one mailbox's builds no Job.
func TestAMailboxIsOneDirectorysName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "../x", "a b", "-rf", "a\nb", "Alice", ".archive", "a..b", strings.Repeat("a", 65)} {
		if ValidMailboxName(bad) == nil {
			t.Errorf("%q is taken for a mailbox's name", bad)
		}
	}
	for _, good := range []string{"alice", "a.b-c_d+e", "0x"} {
		if err := ValidMailboxName(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"", "alice", "alice-2026", "../alice-20261009T143000Z", "alice-20261009T143000Z/x", "a..b-20261009T143000Z"} {
		if ValidArchiveName(bad) == nil {
			t.Errorf("%q is taken for an archived mailbox's name", bad)
		}
	}
	target := personTarget()
	target.Archive = "bob-20261009T143000Z"
	if _, err := MailboxArchiveJob(target); err == nil {
		t.Error("a mailbox would be archived under another mailbox's name")
	}
	target = personTarget()
	target.Domain = MailArchiveDir
	if _, err := MailboxDeleteJob(target); err == nil {
		t.Error("the archive was taken for a mail domain")
	}
}

func TestWhatAMailboxJobReports(t *testing.T) {
	got, err := ParseMailboxOutcome("result=archived\nbytes=20480\nmessages=3\n")
	if err != nil || got != (MailboxOutcome{Result: MailboxArchived, Bytes: 20480, Messages: 3}) {
		t.Errorf("%+v %v", got, err)
	}
	for _, silent := range []string{"", "bytes=3", "result=maybe"} {
		if _, err := ParseMailboxOutcome(silent); err == nil {
			t.Errorf("%q was taken for an outcome", silent)
		}
	}
}

// The archived mailboxes are on the list of what a tenant owns, with the
// mailboxes they were: carried by a backup, put back as archived by a
// restore and an import, destroyed with the tenant.
func TestTheArchivedMailboxesAreOnTheInventory(t *testing.T) {
	rule := TenantRuleFor(bundle.ArtefactMailboxes)
	for field, text := range map[string]string{"what": rule.What, "export": rule.Export, "restore": rule.Restore, "import": rule.Import, "delete": rule.Delete} {
		if !strings.Contains(text, "archived") {
			t.Errorf("the inventory's mailboxes say nothing about the archived ones under %s: %q", field, text)
		}
	}
	if !rule.Destroyed {
		t.Error("the mailboxes are not destroyed with the tenant")
	}
	// And the scripts do what the list says: the destroy script removes the
	// domain's archive, the backup copies it, the restore writes below it.
	if !strings.Contains(mailboxDestroyScript(), MailArchiveDir) {
		t.Error("a tenant's deletion leaves its archived mailboxes")
	}
}
