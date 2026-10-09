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
	"sort"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/gentian-org/gentian-os/internal/kernel"
)

// One person's mailbox, archived and deleted, against mailboxes: the scripts
// the Jobs run, in the mail server's image, as the mail server's user, on a
// volume laid out as the server lays its own out. Run when asked for, like
// the tenant's (mailboxes_live_test.go):
//
//	GENTIAN_LIVE_DOVECOT=1 go test ./internal/backup -run TestOnePersonsMailboxAgainstDovecot -v

// person runs one mailbox Job's script and answers what it printed and what
// it reported as its outcome.
func (m *liveMail) person(job *batchv1.Job, env ...string) (string, MailboxOutcome, error) {
	m.t.Helper()
	c := job.Spec.Template.Spec.Containers[0]
	all := []string{"RESULT=" + workDir + "/result"}
	for _, e := range c.Env {
		all = append(all, e.Name+"="+e.Value)
	}
	// Later values win: a test overrides a name the Job would not be built
	// with.
	all = append(all, env...)
	m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", "rm -f "+workDir+"/result")
	out, err := m.run(DovecotImage, "1000:1000", c.Command[0], c.Args[0], all...)
	if err != nil {
		return out, MailboxOutcome{}, err
	}
	report := m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", "cat "+workDir+"/result")
	outcome, err := ParseMailboxOutcome(report)
	return out, outcome, err
}

// contentsAt is every message of the mailbox a directory is, as contents is
// of a live one.
func (m *liveMail) contentsAt(dir string) string {
	m.t.Helper()
	out := m.mustRun(DovecotImage, "1000:1000", "/bin/bash", doveadmPrelude+
		fmt.Sprintf("dv %s -f tab fetch 'mailbox uid guid flags hdr.subject' all", shellSingleQuote(dir)), "DOMAIN=demo.example")
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(line, `\Recent`, ""), "  ", " "))
		if line != "" && !strings.HasPrefix(line, "mailbox\t") {
			lines = append(lines, line)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// asTheServer runs doveadm for an address the way the mail server opens a
// mailbox: by its own layout, from the address alone.
func (m *liveMail) asTheServer(address, commands string) string {
	m.t.Helper()
	script := `set -euo pipefail
CONF=/tmp/doveadm.conf
cat > "${CONF}" <<'CONF_EOF'
log_path = /dev/stderr
ssl = no
mail_uid = 1000
mail_gid = 1000
first_valid_uid = 1000
first_valid_gid = 1000
mail_location = ` + MailLayout + `
CONF_EOF
srv() { USER="${ADDRESS}" HOME=/tmp doveadm -c "${CONF}" "$@"; }
` + commands
	return m.mustRun(DovecotImage, "1000:1000", "/bin/bash", script, "ADDRESS="+address)
}

func (m *liveMail) tree() string {
	m.t.Helper()
	return m.mustRun(DovecotImage, "1000:1000", "/bin/sh", "cd "+MailRoot+" && find . -mindepth 1 -maxdepth 3 -type d ! -name cur ! -name new ! -name tmp ! -name '.[A-Z]*' | LC_ALL=C sort")
}

func TestOnePersonsMailboxAgainstDovecot(t *testing.T) {
	m := startLiveMail(t)
	deliver := func(domain, box, folder, subject string) {
		m.mailbox(domain, box, fmt.Sprintf(`dv "${box}" mailbox create -s %[1]s 2>/dev/null || true
printf 'From: sender@elsewhere.example\nSubject: %[2]s\n\nthe body of %[2]s\n' | dv "${box}" save -m %[1]s`, folder, subject))
	}
	target := func(box, archive string) MailboxTarget {
		return MailboxTarget{Namespace: "system-mail", Claim: "claim", Tenant: "demo", Record: "demo-x1", Domain: "demo.example", Mailbox: box, Archive: archive}
	}

	// Two people of one tenant with mail, and another tenant's.
	deliver("demo.example", "alice", "INBOX", "to-alice-1")
	deliver("demo.example", "alice", "INBOX", "to-alice-2")
	deliver("demo.example", "alice", "Archive", "filed-by-alice")
	deliver("demo.example", "bob", "INBOX", "to-bob-1")
	deliver("demo.example", "bob", "INBOX", "to-bob-2")
	deliver("other.example", "alice", "INBOX", "to-the-other-alice")
	deliver("other.example", "carol", "INBOX", "to-carol")
	m.mailbox("demo.example", "alice", `dv "${box}" flags add '\Seen \Flagged' mailbox INBOX header subject to-alice-1
dv "${box}" flags add '\Answered $Later' mailbox Archive all`)
	alice := m.contents("demo.example", "alice")
	bob := m.contents("demo.example", "bob")
	otherAlice := m.contents("other.example", "alice")
	carol := m.contents("other.example", "carol")
	if strings.Count(alice, "\n")+1 != 3 || !strings.Contains(alice, `\Flagged`) || !strings.Contains(alice, "$Later") {
		t.Fatalf("alice's mailbox before:\n%s", alice)
	}

	// --- alice is removed, and her mailbox is to be archived.
	name := ArchiveName("alice", time.Date(2026, 10, 9, 14, 30, 0, 0, time.UTC))
	archive, err := MailboxArchiveJob(target("alice", name))
	if err != nil {
		t.Fatal(err)
	}
	out, outcome, err := m.person(archive)
	if err != nil || outcome.Result != MailboxArchived || outcome.Messages != 3 || outcome.Bytes <= 0 {
		t.Fatalf("the archive Job: %v %+v\n%s", err, outcome, out)
	}
	kept := MailRoot + "/" + MailArchiveDir + "/demo.example/" + name
	// The live directory is gone, the archive is there, nobody else's moved.
	want := "./.archive\n./.archive/demo.example\n./.archive/demo.example/" + name + "\n./demo.example\n./demo.example/bob\n./other.example\n./other.example/alice\n./other.example/carol\n"
	if got := m.tree(); got != want {
		t.Fatalf("after the archive the volume holds:\n%s\nwant:\n%s", got, want)
	}
	// The archive is the mailbox: every folder, message, UID, GUID and flag.
	if got := m.contentsAt(kept); got != alice {
		t.Errorf("the archived mailbox:\n%s\nthe mailbox before:\n%s", got, alice)
	}
	for box, before := range map[string][2]string{"bob": {"demo.example", bob}, "alice": {"other.example", otherAlice}, "carol": {"other.example", carol}} {
		if got := m.contents(before[0], box); got != before[1] {
			t.Errorf("archiving alice@demo.example changed %s@%s:\n%s", box, before[0], got)
		}
	}
	// Under her address the mail server finds nothing: the directory its
	// layout gives the address is not there.
	if got := strings.TrimSpace(m.asTheServer("alice@demo.example", "srv search all | wc -l")); got != "0" {
		t.Errorf("the mail server still opens %s message(s) under alice@demo.example", got)
	}
	// That look, as any sign-in would, made an empty mailbox there. It is not
	// the archive, and a second run of the Job leaves both as they are.
	out, outcome, err = m.person(archive)
	if err != nil || outcome.Result != MailboxArchived || !strings.Contains(out, "archived already") {
		t.Fatalf("the archive Job, asked again: %v %+v\n%s", err, outcome, out)
	}
	if got := m.contentsAt(kept); got != alice {
		t.Errorf("a second run changed the archive:\n%s", got)
	}

	// --- the same address is given to a new person: an empty mailbox, and
	// mail delivered to it goes there and not into the archive.
	m.asTheServer("alice@demo.example", `printf 'From: sender@elsewhere.example\nSubject: to-the-new-alice\n\nhello\n' | srv save -m INBOX`)
	fresh := m.contents("demo.example", "alice")
	if strings.Count(fresh, "\n")+1 != 1 || !strings.Contains(fresh, "to-the-new-alice") || strings.Contains(fresh, "to-alice-1") {
		t.Errorf("the new alice@demo.example does not start empty:\n%s", fresh)
	}
	if got := m.contentsAt(kept); got != alice {
		t.Errorf("mail to the new alice changed the archive:\n%s", got)
	}

	// --- a person with no mailbox yet: nothing to archive, said as that.
	nobody, err := MailboxArchiveJob(target("dora", ArchiveName("dora", time.Now())))
	if err != nil {
		t.Fatal(err)
	}
	if out, outcome, err = m.person(nobody); err != nil || outcome.Result != MailboxAbsent {
		t.Errorf("archiving a mailbox that is not there: %v %+v\n%s", err, outcome, out)
	}

	// --- the tenant's backup holds the archive, as an archive.
	p, d := mailParams()
	backupJob := MailboxBackupJob(p, "claim", "demo.example")
	out = m.mustRun(DovecotImage, "1000:1000", "/bin/bash", containerByName(backupJob, "mailbox-backup").Args[0], "DOMAIN=demo.example")
	if !strings.Contains(out, "copied 2 mailbox(es) of demo.example") || !strings.Contains(out, "copied 1 archived mailbox(es) of demo.example") {
		t.Fatalf("the backup:\n%s", out)
	}
	m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", containerByName(backupJob, "pack-mailboxes").Args[0])
	if index := m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", "tar xzOf "+workDir+"/mailboxes.tar.gz ./INDEX"); index != "alice\nbob\n" {
		t.Errorf("the bundle lists the live mailboxes %q", index)
	}
	if index := m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", "tar xzOf "+workDir+"/mailboxes.tar.gz ./archived/INDEX"); index != name+"\n" {
		t.Errorf("the bundle lists the archived mailboxes %q", index)
	}

	// --- bob is removed, and his mailbox is to be deleted.
	remove, err := MailboxDeleteJob(target("bob", ""))
	if err != nil {
		t.Fatal(err)
	}
	out, outcome, err = m.person(remove)
	if err != nil || outcome.Result != MailboxDeleted || outcome.Messages != 2 {
		t.Fatalf("the delete Job: %v %+v\n%s", err, outcome, out)
	}
	if got := m.tree(); strings.Contains(got, "bob") {
		t.Fatalf("bob's mailbox is still there:\n%s", got)
	}
	if out, outcome, err = m.person(remove); err != nil || outcome.Result != MailboxAbsent {
		t.Errorf("the delete Job, asked again: %v %+v\n%s", err, outcome, out)
	}
	if got := m.contentsAt(kept); got != alice {
		t.Errorf("deleting bob's mailbox changed alice's archive")
	}

	// --- names that are not one mailbox's are refused, before a Job is
	// built and by the script, and nothing is removed.
	for _, bad := range []string{"", ".", "..", "../other.example/carol", "a/b", "-rf", "Alice", ".archive", "a b"} {
		if _, err := MailboxDeleteJob(target(bad, "")); err == nil {
			t.Errorf("a Job was built to delete the mailbox %q", bad)
		}
		if _, err := MailboxArchiveJob(target(bad, bad+"-20261009T143000Z")); err == nil {
			t.Errorf("a Job was built to archive the mailbox %q", bad)
		}
	}
	for _, bad := range []string{"", "..", "alice", "../../other.example", "alice-20261009T143000Z/..", "x-20261009"} {
		if _, err := ArchivedMailboxDeleteJob(target("", bad)); err == nil {
			t.Errorf("a Job was built to delete the archive %q", bad)
		}
	}
	purge, err := ArchivedMailboxDeleteJob(target("", name))
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range [][]string{
		{"BOX="}, {"BOX=."}, {"BOX=.."}, {"BOX=../other.example/carol"}, {"BOX=x/../../other.example/carol"},
		{"DOMAIN="}, {"DOMAIN=.."}, {"DOMAIN=.archive"}, {"DOMAIN=other.example/carol", "BOX=cur"},
	} {
		if out, _, err := m.person(remove, env...); err == nil || !strings.Contains(out, "refused") {
			t.Errorf("the delete script accepted %v: %v\n%s", env, err, out)
		}
		if out, _, err := m.person(archive, env...); err == nil || !strings.Contains(out, "refused") {
			t.Errorf("the archive script accepted %v: %v\n%s", env, err, out)
		}
	}
	for _, env := range [][]string{{"NAME="}, {"NAME=.."}, {"NAME=../../other.example"}, {"NAME=x/../../../other.example/carol"}, {"DOMAIN=.."}} {
		if out, _, err := m.person(purge, env...); err == nil || !strings.Contains(out, "refused") {
			t.Errorf("the script that deletes an archive accepted %v: %v\n%s", env, err, out)
		}
	}
	// A link where a mailbox, a domain or the archive should be is not
	// followed: what it points at is somebody else's.
	m.mustRun(DovecotImage, "1000:1000", "/bin/sh", "ln -s "+MailRoot+"/other.example/carol "+MailRoot+"/demo.example/evil && ln -s "+MailRoot+"/other.example "+MailRoot+"/linked.example")
	if out, _, err := m.person(remove, "BOX=evil"); err == nil || !strings.Contains(out, "is a link") {
		t.Errorf("a mailbox that is a link was deleted through: %v\n%s", err, out)
	}
	if out, _, err := m.person(archive, "BOX=evil", "NAME=evil-20261009T143000Z"); err == nil || !strings.Contains(out, "is a link") {
		t.Errorf("a mailbox that is a link was archived: %v\n%s", err, out)
	}
	if out, _, err := m.person(remove, "DOMAIN=linked.example", "BOX=carol"); err == nil || !strings.Contains(out, "is a link") {
		t.Errorf("a domain that is a link was worked through: %v\n%s", err, out)
	}
	m.mustRun(DovecotImage, "1000:1000", "/bin/sh", "rm "+MailRoot+"/demo.example/evil "+MailRoot+"/linked.example")
	for box, before := range map[string]string{"alice": otherAlice, "carol": carol} {
		if got := m.contents("other.example", box); got != before {
			t.Errorf("a refused Job changed %s@other.example:\n%s", box, got)
		}
	}
	if got := m.contentsAt(kept); got != alice {
		t.Errorf("a refused Job changed the archive")
	}

	// --- the tenant is deleted: its mailboxes go, and its archive with them.
	// Then the bundle is imported under another domain: the archive comes
	// back as an archive, and is nobody's live mailbox.
	destroy := MailboxDestroyJob("system-mail", "demo", "claim", "", "demo.example", DestroyInTheBackground)
	out = m.mustRun(DovecotImage, "1000:1000", "/bin/sh", destroy.Spec.Template.Spec.Containers[0].Args[0], "DOMAIN=demo.example")
	if !strings.Contains(out, "the archived mailboxes of demo.example are gone") {
		t.Fatalf("the tenant's deletion:\n%s", out)
	}
	if got := m.tree(); got != "./.archive\n./other.example\n./other.example/alice\n./other.example/carol\n" {
		t.Fatalf("after the tenant's deletion the volume holds:\n%s", got)
	}
	restore := MailboxRestoreJob(p, d, MailboxesArtefact, "claim", "renamed.example")
	m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", containerByName(restore, "unpack-mailboxes").Args[0])
	out = m.mustRun(DovecotImage, "1000:1000", "/bin/bash", containerByName(restore, "mailbox-restore").Args[0], "DOMAIN=renamed.example")
	if !strings.Contains(out, "restored 2 mailbox(es) into renamed.example") || !strings.Contains(out, "restored 1 archived mailbox(es) of renamed.example") {
		t.Fatalf("the restore:\n%s", out)
	}
	if got := m.contentsAt(MailRoot + "/" + MailArchiveDir + "/renamed.example/" + name); got != alice {
		t.Errorf("the archive after the import:\n%s\nwant:\n%s", got, alice)
	}
	// The live alice of the bundle is the new one; the archived mail is in
	// no live mailbox.
	if got := m.contents("renamed.example", "alice"); got != fresh {
		t.Errorf("the imported live mailbox of alice:\n%s\nwant:\n%s", got, fresh)
	}

	// --- the archived mailbox is deleted, on purpose, later.
	purge, err = ArchivedMailboxDeleteJob(MailboxTarget{Namespace: "system-mail", Claim: "claim", Tenant: "demo", Record: "demo-x1", Domain: "renamed.example", Archive: name})
	if err != nil {
		t.Fatal(err)
	}
	out, outcome, err = m.person(purge)
	if err != nil || outcome.Result != MailboxDeleted || outcome.Messages != 3 {
		t.Fatalf("deleting the archived mailbox: %v %+v\n%s", err, outcome, out)
	}
	if got := m.tree(); strings.Contains(got, name) || !strings.Contains(got, "./renamed.example/alice") {
		t.Errorf("after deleting the archived mailbox the volume holds:\n%s", got)
	}
}
