/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// One person's mailbox, when the person is removed.
//
// Whoever removes a person says what becomes of the mailbox: it is archived,
// or it is deleted. Both are done here, by a Job beside the mail server's
// volume, as the tenant's mailbox Jobs are (mailboxes.go).
//
// Archived means moved: out of MailRoot/<domain>/<name>, where the mail
// server opens a mailbox for whoever signs in under that address, to
// MailRoot/.archive/<domain>/<name>-<when>. The move is one rename on one
// volume, so the archive is the mailbox itself -- every folder, message,
// flag and UID as the server left them -- and at no moment is there half of
// it in either place. Nothing opens it there: no address maps to a directory
// below .archive (a mail domain's name never begins with a dot,
// ValidMailDomain), and the next person given the same address starts with
// no directory at all, which the server makes empty at the first delivery.
//
// Deleted means removed, and gone by the volume's own listing before the Job
// says so.
//
// The names come from an address and from a record somebody else wrote, so
// each is checked twice: before a Job is built (ValidMailboxName,
// ValidArchiveName), and by the script itself, which also refuses to work
// through a link.

// MailArchiveDir is the directory below MailRoot that archived mailboxes are
// kept in, one directory per mail domain.
const MailArchiveDir = ".archive"

// archivedInBundle is where the archive of a tenant's mailboxes keeps the
// archived ones: beside the live ones, under an INDEX of their own.
const archivedInBundle = "archived"

// mailboxName is what the part of an address before the @ may be for its
// mailbox to be moved or deleted by name. Narrower than what an address
// allows, on purpose: a name outside it is refused and its mailbox left
// where it is.
var mailboxName = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`)

// archiveStamp is the form of the moment in an archive's name.
const archiveStamp = "20060102T150405Z"

var archiveName = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}-[0-9]{8}T[0-9]{6}Z$`)

// ValidMailboxName reports why a name cannot be one mailbox's directory, or
// nil.
func ValidMailboxName(name string) error {
	if !mailboxName.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%q is not a name one mailbox's directory can have here: lower-case letters, digits, dots, dashes, underscores and plus signs, beginning with a letter or a digit", name)
	}
	return nil
}

// ArchiveName is the directory an archived mailbox is kept as: the
// mailbox's name and the moment it was archived at.
func ArchiveName(mailbox string, at time.Time) string {
	return mailbox + "-" + at.UTC().Format(archiveStamp)
}

// ValidArchiveName reports why a name cannot be an archived mailbox's
// directory, or nil.
func ValidArchiveName(name string) error {
	if !archiveName.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%q is not the name of an archived mailbox", name)
	}
	return nil
}

// The three things a Job does for a record.
const (
	MailboxActArchive       = "archive"
	MailboxActDelete        = "delete"
	MailboxActDeleteArchive = "delete-archive"
)

// MailboxJobName names the Job that does one thing for one record. A
// record's name is as long as a tenant's and more, and a Job's is held to a
// label's length, so the record goes in as a digest: the Job carries the
// record's name itself as an annotation (MailboxRecordAnnotation).
func MailboxJobName(record, act string) string {
	sum := sha256.Sum256([]byte(record))
	return "mailbox-" + hex.EncodeToString(sum[:])[:12] + "-" + act
}

// personPrelude is what the three scripts begin with: the names checked, and
// every directory on the way refused if it is a link -- a link below the
// mail root would make "this mailbox" somebody else's.
const personPrelude = `set -eu
ROOT=%[1]s
ARCHIVE="${ROOT}/%[2]s"
refuse() { echo "ERROR: refused: $1" >&2; exit 1; }
case "${DOMAIN}" in
  ""|.*|-*|*/*|*[[:cntrl:]]*|*[[:space:]]*) refuse "${DOMAIN} is not a mail domain's directory" ;;
esac
plain() {
  case "$1" in
    ""|.*|-*|*/*|*..*|*[[:cntrl:]]*|*[[:space:]]*) refuse "$1 is not a mailbox's directory" ;;
  esac
}
no_link() {
  if [ -L "$1" ]; then refuse "$1 is a link"; fi
}
# gone PARENT NAME: by the parent's own listing, which has to be readable.
gone() {
  if [ -e "$1" ] || [ -L "$1" ]; then
    listing="$(ls -A "$1")"
    if printf '%%s\n' "${listing}" | grep -qxF -- "$2"; then return 1; fi
  fi
  return 0
}
# measure DIRECTORY: its size in bytes and the messages in it.
measure() {
  bytes="$(du -sk "$1" | cut -f1)"
  bytes=$((bytes * 1024))
  find "$1" -type f \( -path '*/cur/*' -o -path '*/new/*' \) > /tmp/messages
  messages="$(wc -l < /tmp/messages)"
}
# result WHAT: for whoever reads the Job's outcome.
result() {
  printf 'result=%%s\nbytes=%%s\nmessages=%%s\n' "$1" "${bytes:-0}" "${messages:-0}" > "${RESULT:-/dev/termination-log}"
}
ls -A "${ROOT}" >/dev/null
no_link "${ROOT}/${DOMAIN}"
no_link "${ARCHIVE}"
no_link "${ARCHIVE}/${DOMAIN}"
`

// Results a mailbox Job reports (MailboxOutcome.Result).
const (
	// MailboxArchived: the mailbox is in the archive.
	MailboxArchived = "archived"
	// MailboxDeleted: the mailbox, or the archived mailbox, is gone.
	MailboxDeleted = "deleted"
	// MailboxAbsent: there was no such directory. An address has a mailbox
	// from the first message delivered to it and none before.
	MailboxAbsent = "none"
)

func mailboxArchiveScript() string {
	return fmt.Sprintf(personPrelude+`plain "${BOX}"
plain "${NAME}"
case "${NAME}" in
  "${BOX}"-*) ;;
  *) refuse "${NAME} is not an archive of ${BOX}" ;;
esac
live="${ROOT}/${DOMAIN}/${BOX}"
kept="${ARCHIVE}/${DOMAIN}/${NAME}"
no_link "${live}"
no_link "${kept}"
if [ -d "${kept}" ]; then
  # Asked a second time: the move was made, and what has arrived at the
  # address since is not this person's and is left where it is.
  echo "${BOX}@${DOMAIN} is archived already, as ${NAME}"
elif [ ! -e "${live}" ]; then
  echo "${BOX}@${DOMAIN} has no mailbox: nothing was ever delivered to it"
  result none
  exit 0
else
  [ -d "${live}" ] || refuse "${live} is not a directory"
  mkdir -p "${ARCHIVE}/${DOMAIN}"
  chmod 0700 "${ARCHIVE}"
  # One rename, on one volume: the archive is the mailbox, whole.
  mv -- "${live}" "${kept}"
  if [ -e "${live}" ] && [ ! -d "${kept}" ]; then
    echo "ERROR: ${BOX}@${DOMAIN} was not moved" >&2; exit 1
  fi
  echo "${BOX}@${DOMAIN} is archived as ${NAME}"
fi
[ -d "${kept}" ] || { echo "ERROR: the archive ${NAME} of ${DOMAIN} is not there" >&2; exit 1; }
measure "${kept}"
echo "the archive holds ${messages} message(s), ${bytes} bytes"
result archived
`, MailRoot, MailArchiveDir)
}

func mailboxDeleteScript() string {
	return fmt.Sprintf(personPrelude+`plain "${BOX}"
live="${ROOT}/${DOMAIN}/${BOX}"
no_link "${live}"
if [ ! -e "${live}" ]; then
  echo "${BOX}@${DOMAIN} has no mailbox: nothing was ever delivered to it"
  result none
  exit 0
fi
[ -d "${live}" ] || refuse "${live} is not a directory"
measure "${live}"
rm -rf -- "${ROOT:?}/${DOMAIN:?}/${BOX:?}"
if ! gone "${ROOT}/${DOMAIN}" "${BOX}"; then
  echo "ERROR: the mailbox of ${BOX}@${DOMAIN} is still there" >&2; exit 1
fi
echo "the mailbox of ${BOX}@${DOMAIN} is gone: ${messages} message(s), ${bytes} bytes"
result deleted
`, MailRoot, MailArchiveDir)
}

func archivedMailboxDeleteScript() string {
	return fmt.Sprintf(personPrelude+`plain "${NAME}"
kept="${ARCHIVE}/${DOMAIN}/${NAME}"
no_link "${kept}"
if [ ! -e "${kept}" ]; then
  echo "${DOMAIN} has no archived mailbox ${NAME}"
  result none
  exit 0
fi
[ -d "${kept}" ] || refuse "${kept} is not a directory"
measure "${kept}"
rm -rf -- "${ARCHIVE:?}/${DOMAIN:?}/${NAME:?}"
if ! gone "${ARCHIVE}/${DOMAIN}" "${NAME}"; then
  echo "ERROR: the archived mailbox ${NAME} of ${DOMAIN} is still there" >&2; exit 1
fi
echo "the archived mailbox ${NAME} of ${DOMAIN} is gone: ${messages} message(s), ${bytes} bytes"
result deleted
`, MailRoot, MailArchiveDir)
}

// MailboxTarget says which mailbox a Job works on and where its volume is.
type MailboxTarget struct {
	// Namespace is the mail server's; Claim its volume; Node the node the
	// server holds the volume on, "" when nothing holds it.
	Namespace, Claim, Node string
	// Tenant is whose person the mailbox was; Record the record that asked.
	Tenant, Record string
	// Domain and Mailbox are the address: the mail domain and the part
	// before the @.
	Domain, Mailbox string
	// Archive is the archived mailbox's directory name (ArchiveName).
	Archive string
}

func (t MailboxTarget) job(act, container, script string, env ...corev1.EnvVar) *batchv1.Job {
	c := mailContainer(container, script, append([]corev1.EnvVar{{Name: "DOMAIN", Value: t.Domain}}, env...)...)
	c.Command = []string{"/bin/sh", "-c"}
	context := c.SecurityContext
	job := destroyJob(t.Namespace, MailboxJobName(t.Record, act), t.Tenant, mailStore, DestroyInTheBackground, c)
	spec := &job.Spec.Template.Spec
	spec.Containers[0].SecurityContext = context
	spec.Volumes = []corev1.Volume{mailVolume(t.Claim)}
	spec.NodeSelector = nodeSelectorFor(t.Node)
	job.Annotations = map[string]string{MailboxRecordAnnotation: t.Record}
	return job
}

// MailboxRecordAnnotation names, on a Job, the record that asked for it.
const MailboxRecordAnnotation = "gentianos.io/mailbox-removal"

// MailboxArchiveJob moves one mailbox into the archive.
func MailboxArchiveJob(t MailboxTarget) (*batchv1.Job, error) {
	if err := t.valid(true, true); err != nil {
		return nil, err
	}
	return t.job(MailboxActArchive, "archive-mailbox", mailboxArchiveScript(),
		corev1.EnvVar{Name: "BOX", Value: t.Mailbox}, corev1.EnvVar{Name: "NAME", Value: t.Archive}), nil
}

// MailboxDeleteJob deletes one mailbox.
func MailboxDeleteJob(t MailboxTarget) (*batchv1.Job, error) {
	if err := t.valid(true, false); err != nil {
		return nil, err
	}
	return t.job(MailboxActDelete, "delete-mailbox", mailboxDeleteScript(), corev1.EnvVar{Name: "BOX", Value: t.Mailbox}), nil
}

// ArchivedMailboxDeleteJob deletes one archived mailbox.
func ArchivedMailboxDeleteJob(t MailboxTarget) (*batchv1.Job, error) {
	if err := t.valid(false, true); err != nil {
		return nil, err
	}
	return t.job(MailboxActDeleteArchive, "delete-archived-mailbox", archivedMailboxDeleteScript(), corev1.EnvVar{Name: "NAME", Value: t.Archive}), nil
}

func (t MailboxTarget) valid(mailbox, archive bool) error {
	if err := ValidMailDomain(t.Domain); err != nil {
		return err
	}
	if mailbox {
		if err := ValidMailboxName(t.Mailbox); err != nil {
			return err
		}
	}
	if archive {
		if err := ValidArchiveName(t.Archive); err != nil {
			return err
		}
		if mailbox && !strings.HasPrefix(t.Archive, t.Mailbox+"-") {
			return fmt.Errorf("%q is not an archive of the mailbox %q", t.Archive, t.Mailbox)
		}
	}
	return nil
}

// MailboxOutcome is what a mailbox Job reported when it ended.
type MailboxOutcome struct {
	// Result is one of MailboxArchived, MailboxDeleted, MailboxAbsent.
	Result string
	// Bytes and Messages are what the mailbox held.
	Bytes, Messages int64
}

// ParseMailboxOutcome reads what a Job's container left as its termination
// message. A message that does not say what was done is an error: a Job that
// succeeded and said nothing is not taken for one that did its work.
func ParseMailboxOutcome(message string) (MailboxOutcome, error) {
	var out MailboxOutcome
	for _, line := range strings.Split(message, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "result":
			out.Result = value
		case "bytes":
			out.Bytes, _ = strconv.ParseInt(value, 10, 64)
		case "messages":
			out.Messages, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	switch out.Result {
	case MailboxArchived, MailboxDeleted, MailboxAbsent:
		return out, nil
	}
	return out, fmt.Errorf("the Job did not say what it did: %q", strings.TrimSpace(message))
}
