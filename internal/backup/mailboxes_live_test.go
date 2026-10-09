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
	"sort"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel"
)

// The mailbox scripts against mailboxes.
//
// What a copy of a mailbox holds is decided by doveadm, not by the text of
// a script, so this runs the backup, destroy and restore scripts the Jobs
// run, in the mail server's image, as the mail server's user, on a volume
// laid out as the server lays its own out. It needs docker and is run when
// asked for:
//
//	GENTIAN_LIVE_DOVECOT=1 go test ./internal/backup -run TestMailboxesAgainstDovecot -v

type liveMail struct {
	t          *testing.T
	mail, work string
}

func startLiveMail(t *testing.T) *liveMail {
	t.Helper()
	if os.Getenv("GENTIAN_LIVE_DOVECOT") == "" {
		t.Skip("set GENTIAN_LIVE_DOVECOT=1 to run the mailbox scripts in the mail server's image in docker")
	}
	m := &liveMail{t: t, mail: fmt.Sprintf("gentian-mail-live-%d", os.Getpid()), work: fmt.Sprintf("gentian-mail-live-%d-work", os.Getpid())}
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", m.mail, m.work).Run() })
	// The volume as the cluster hands it to the mail server: the mail user's
	// (fsGroup), and not empty, so that docker does not lay the image's own
	// directory over it. The scratch directory as an emptyDir is: anybody's.
	m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh",
		"chown 1000:1000 /var/mail && touch /var/mail/.volume && chmod 0777 /work")
	return m
}

// run runs a script as a Job's container would: image, user, both volumes.
func (m *liveMail) run(image, user, shell, script string, env ...string) (string, error) {
	args := []string{"run", "--rm", "-i", "--user", user, "-v", m.mail + ":" + MailRoot, "-v", m.work + ":" + workDir}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, "--entrypoint", shell, image, "-c", script)
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

func (m *liveMail) mustRun(image, user, shell, script string, env ...string) string {
	m.t.Helper()
	out, err := m.run(image, user, shell, script, env...)
	if err != nil {
		m.t.Fatalf("the script failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}
	return out
}

// mailbox runs doveadm on one mailbox, the way the scripts do.
func (m *liveMail) mailbox(domain, box, commands string) string {
	m.t.Helper()
	return m.mustRun(DovecotImage, "1000:1000", "/bin/bash", doveadmPrelude+
		fmt.Sprintf("box=%s/${DOMAIN}/%s\n", MailRoot, box)+commands, "DOMAIN="+domain)
}

// contents is every message of a mailbox: folder, UID, GUID, flags and
// subject, in order. \Recent is the session's, not the message's.
func (m *liveMail) contents(domain, box string) string {
	m.t.Helper()
	out := m.mailbox(domain, box, `dv "${box}" -f tab fetch 'mailbox uid guid flags hdr.subject' all`)
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

func (m *liveMail) listing() string {
	m.t.Helper()
	return m.mustRun(DovecotImage, "1000:1000", "/bin/sh", "cd "+MailRoot+" && find . -mindepth 1 -maxdepth 2 -type d | LC_ALL=C sort")
}

func TestMailboxesAgainstDovecot(t *testing.T) {
	m := startLiveMail(t)
	p, d := mailParams()
	deliver := func(domain, box, folder, subject string) {
		m.mailbox(domain, box, fmt.Sprintf(`dv "${box}" mailbox create -s %[1]s 2>/dev/null || true
printf 'From: sender@elsewhere.example\nSubject: %[2]s\n\nthe body of %[2]s\n' | dv "${box}" save -m %[1]s`, folder, subject))
	}

	// Two tenants' mailboxes, with mail in more than one folder and flags.
	deliver("demo.example", "alice", "INBOX", "to-alice-1")
	deliver("demo.example", "alice", "INBOX", "to-alice-2")
	deliver("demo.example", "alice", "Archive", "filed-by-alice")
	deliver("demo.example", "bob", "INBOX", "to-bob")
	deliver("other.example", "carol", "INBOX", "to-carol")
	deliver("other.example", "alice", "INBOX", "to-the-other-alice")
	m.mailbox("demo.example", "alice", `dv "${box}" flags add '\Seen \Flagged' mailbox INBOX header subject to-alice-1
dv "${box}" flags add '\Answered $Later' mailbox Archive all`)
	m.mailbox("other.example", "carol", `dv "${box}" flags add '\Seen' mailbox INBOX all`)

	before := map[string]string{}
	for _, box := range [][2]string{{"demo.example", "alice"}, {"demo.example", "bob"}, {"other.example", "carol"}, {"other.example", "alice"}} {
		before[box[1]+"@"+box[0]] = m.contents(box[0], box[1])
	}
	if got := strings.Count(before["alice@demo.example"], "\n") + 1; got != 3 {
		t.Fatalf("alice@demo.example holds %d message(s) before the backup:\n%s", got, before["alice@demo.example"])
	}
	for _, want := range []string{`\Flagged`, `\Seen`, `\Answered`, "$Later"} {
		if !strings.Contains(before["alice@demo.example"], want) {
			t.Fatalf("the flag %s is not on alice's mail before the backup:\n%s", want, before["alice@demo.example"])
		}
	}

	// --- back up one tenant's: the Job's two producing containers.
	job := MailboxBackupJob(p, "claim", "demo.example")
	out := m.mustRun(DovecotImage, "1000:1000", "/bin/bash", containerByName(job, "mailbox-backup").Args[0], "DOMAIN=demo.example")
	if !strings.Contains(out, "copied 2 mailbox(es) of demo.example") {
		t.Fatalf("the backup did not copy the tenant's two mailboxes:\n%s", out)
	}
	m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", containerByName(job, "pack-mailboxes").Args[0])
	index := m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", "tar xzOf "+workDir+"/mailboxes.tar.gz ./INDEX")
	if index != "alice\nbob\n" {
		t.Fatalf("the archive lists %q", index)
	}
	// The other tenant's mail is nowhere in the archive.
	if names := m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", "tar tzf "+workDir+"/mailboxes.tar.gz"); strings.Contains(names, "carol") {
		t.Errorf("the archive of demo.example names another tenant's mailbox:\n%s", names)
	}
	leaked := m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh",
		"mkdir -p /tmp/x && tar xzf "+workDir+"/mailboxes.tar.gz -C /tmp/x && { grep -rl -e to-carol -e to-the-other-alice /tmp/x || true; }")
	if strings.TrimSpace(leaked) != "" {
		t.Errorf("the archive of demo.example holds another tenant's mail: %s", leaked)
	}

	// --- purge it: the destroy Job's script. The other tenant is untouched.
	destroy := MailboxDestroyJob("system-mail", "demo", "claim", "", "demo.example", DestroyInTheBackground)
	script := destroy.Spec.Template.Spec.Containers[0].Args[0]
	out = m.mustRun(DovecotImage, "1000:1000", "/bin/sh", script, "DOMAIN=demo.example")
	if !strings.Contains(out, "the mailboxes of demo.example are gone") {
		t.Fatalf("the destroy script did not say the mailboxes are gone:\n%s", out)
	}
	if got := m.listing(); got != "./other.example\n./other.example/alice\n./other.example/carol\n" {
		t.Fatalf("after the purge the volume holds:\n%s", got)
	}
	for _, box := range []string{"carol", "alice"} {
		if got := m.contents("other.example", box); got != before[box+"@other.example"] {
			t.Errorf("the purge of demo.example changed %s@other.example:\n%s\nwas:\n%s", box, got, before[box+"@other.example"])
		}
	}
	// Again: nothing there is success, said as that.
	if out = m.mustRun(DovecotImage, "1000:1000", "/bin/sh", script, "DOMAIN=demo.example"); !strings.Contains(out, "demo.example has no mailboxes") {
		t.Errorf("a second purge:\n%s", out)
	}
	// A name that is not a domain's directory destroys nothing.
	for _, bad := range []string{"", ".", "..", "other.example/carol"} {
		if out, err := m.run(DovecotImage, "1000:1000", "/bin/sh", script, "DOMAIN="+bad); err == nil {
			t.Errorf("the destroy script accepted the domain %q:\n%s", bad, out)
		}
	}
	if got := m.listing(); !strings.Contains(got, "./other.example/carol") {
		t.Fatalf("a refused purge removed something:\n%s", got)
	}

	// --- restore: the Job's unpack and synchronise containers.
	restore := MailboxRestoreJob(p, d, MailboxesArtefact, "claim", "demo.example")
	m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", containerByName(restore, "unpack-mailboxes").Args[0])
	out = m.mustRun(DovecotImage, "1000:1000", "/bin/bash", containerByName(restore, "mailbox-restore").Args[0], "DOMAIN=demo.example")
	if !strings.Contains(out, "restored 2 mailbox(es) into demo.example") {
		t.Fatalf("the restore:\n%s", out)
	}
	for _, box := range []string{"alice", "bob"} {
		if got := m.contents("demo.example", box); got != before[box+"@demo.example"] {
			t.Errorf("%s@demo.example after the restore:\n%s\nbefore the backup:\n%s", box, got, before[box+"@demo.example"])
		}
	}
	for _, box := range []string{"carol", "alice"} {
		if got := m.contents("other.example", box); got != before[box+"@other.example"] {
			t.Errorf("the restore of demo.example changed %s@other.example", box)
		}
	}

	// --- a restore is on top: mail that arrived since stays, and what the
	// bundle holds is not added twice.
	deliver("demo.example", "alice", "INBOX", "arrived-since")
	out = m.mustRun(DovecotImage, "1000:1000", "/bin/bash", containerByName(restore, "mailbox-restore").Args[0], "DOMAIN=demo.example")
	after := m.contents("demo.example", "alice")
	if strings.Count(after, "\n")+1 != 4 || !strings.Contains(after, "arrived-since") {
		t.Errorf("a second restore over a mailbox with new mail left:\n%s\n%s", after, out)
	}

	// --- into another domain: an import under another name.
	m.mustRun(DovecotImage, "1000:1000", "/bin/bash", containerByName(restore, "mailbox-restore").Args[0], "DOMAIN=renamed.example")
	if got := m.contents("renamed.example", "bob"); got != before["bob@demo.example"] {
		t.Errorf("bob's mail in the renamed tenant's domain:\n%s\nwant:\n%s", got, before["bob@demo.example"])
	}

	// --- an archive that names a mailbox outside the domain, or holds a
	// link, is refused and writes nothing.
	m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh",
		"printf '../other.example/carol\\n' > "+workDir+"/mail/INDEX")
	if out, err := m.run(DovecotImage, "1000:1000", "/bin/bash", containerByName(restore, "mailbox-restore").Args[0], "DOMAIN=demo.example"); err == nil || !strings.Contains(out, "refused") {
		t.Errorf("a mailbox name that leaves the domain was not refused: %v\n%s", err, out)
	}
	if got := m.contents("other.example", "carol"); got != before["carol@other.example"] {
		t.Error("a refused restore wrote into another tenant's mailbox")
	}
	m.mustRun(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh",
		"cd "+workDir+" && rm -rf mail && mkdir -p src/0 && printf 'x\\n' > src/INDEX && ln -s "+MailRoot+"/other.example/carol/cur src/0/cur && tar czf mailboxes.tar.gz -C src . && rm -rf src")
	if out, err := m.run(kernel.DefaultKeycloakProvisionerImage, "0:0", "sh", containerByName(restore, "unpack-mailboxes").Args[0]); err == nil || !strings.Contains(out, "refused") {
		t.Errorf("an archive holding a link was not refused: %v\n%s", err, out)
	}
}
