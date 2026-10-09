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
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/gentian-org/gentian-os/internal/kernel"
)

// A tenant's mailboxes.
//
// Where the cluster runs its own mail server, every mailbox of every tenant
// is a directory on that server's one volume: MailRoot/<domain>/<the part of
// the address before the @>, the mailbox's home and its Maildir at once
// (kernel/services/dovecot). Nothing else says a mailbox exists -- the
// server looks nobody up, an address has a mailbox from the first message
// delivered to it -- so a tenant's mailboxes are the directories below its
// mail domain's, and the three Jobs here work on exactly that: one copies
// them into a bundle, one puts them back, one destroys them.
//
// They run beside the mail server, which is the only place its volume
// mounts, on the node the server holds it on, and as the user the server
// keeps mail as. They need nothing else of the server: not its
// configuration, not its sign-in, and nothing of how mail is routed to it.
//
// How a mailbox is copied. A Maildir can be read while the server writes to
// it, and an archive of one taken that way is still not a copy of it: a
// message being moved from new/ to cur/, or renamed because a flag changed,
// is in neither listing for an instant, and an archiver that reads the two
// directories one after the other leaves it out without an error. The mail
// server's own synchronisation does not have the problem: `doveadm backup`
// opens the mailbox the way the server does, under the locks the server
// takes, and writes every folder and message with its flags, its UID and its
// GUID. So that is what copies a mailbox here, in the image and version the
// cluster's mail server runs, and the same tool puts it back.
//
// doveadm runs without a server of its own: given no user to look up, it
// takes the mailbox from its settings, and each call names the directory
// (dv below). That is also why the Jobs do not depend on how the server maps
// an address to a directory beyond the one layout above.

// DovecotImage is the mail server's image, the one its chart pins
// (kernel/services/dovecot). The Jobs run the doveadm of the server they
// work beside; a test holds the two to the same tag.
const DovecotImage = "dovecot/dovecot:2.3.21"

// MailRoot is where the mail server's volume is mounted, and MailLayout how
// it keeps a mailbox below it: the server's mail_location. A test holds the
// chart to both.
const (
	MailRoot   = "/var/mail"
	MailLayout = "maildir:/var/mail/%d/%n"
)

// MailUser is the user and group the mail server keeps mail as (its
// mail_uid and mail_gid). A mailbox written as anybody else is one the
// server cannot open.
const MailUser int64 = 1000

// MailboxesArtefact is where a bundle keeps a tenant's mailboxes.
const MailboxesArtefact = "mail/mailboxes.tar.gz"

// MailboxDestroyJobName names the Job that destroys a tenant's mailboxes.
func MailboxDestroyJobName(tenantName string) string {
	return "mail-delete-" + tenantName
}

// ValidMailDomain reports why a name cannot be the directory of a mail
// domain, or nil. The Jobs below remove and write below MailRoot/<domain>:
// a name that is empty, that names the root or its parent, or that has a
// separator in it, would make that somewhere else. No name begins with a
// dot: MailRoot/.archive is where the mailboxes of removed people are kept
// (mailbox_person.go), and is no domain's.
func ValidMailDomain(domain string) error {
	if domain == "" || strings.HasPrefix(domain, ".") || strings.ContainsAny(domain, "/\\\x00\n\r\t ") || strings.HasPrefix(domain, "-") {
		return fmt.Errorf("%q is not a name a mail domain's directory can have", domain)
	}
	return nil
}

// doveadmPrelude is what the two Jobs that read and write mailboxes begin
// with: the settings doveadm runs with, and dv, the one way either opens a
// mailbox -- by its directory.
//
// The settings are the server's as far as a mailbox's files go: the user
// mail is kept as, and the locks. Nothing of its sign-in or its listeners is
// here, and no certificate is read.
const doveadmPrelude = `set -euo pipefail
# A failure inside $(...) stops the script too: count is called that way.
shopt -s inherit_errexit
CONF=/tmp/doveadm.conf
cat > "${CONF}" <<'CONF_EOF'
log_path = /dev/stderr
ssl = no
mail_uid = 1000
mail_gid = 1000
first_valid_uid = 1000
first_valid_gid = 1000
CONF_EOF
# dv DIRECTORY ARGS...: doveadm on the mailbox that directory is.
dv() {
  _home="$1"; shift
  USER="mailbox@${DOMAIN}" HOME="${_home}" doveadm -c "${CONF}" -o "mail_location=maildir:${_home}" "$@"
}
# count DIRECTORY: how many messages the mailbox holds, in every folder.
count() {
  dv "$1" search all > /tmp/found
  wc -l < /tmp/found
}
`

func mailContainer(name, script string, env ...corev1.EnvVar) corev1.Container {
	user := MailUser
	no := false
	yes := true
	return corev1.Container{
		Name:    name,
		Image:   DovecotImage,
		Command: []string{"/bin/bash", "-c"},
		Args:    []string{script},
		Env:     env,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &user,
			RunAsGroup:               &user,
			RunAsNonRoot:             &yes,
			AllowPrivilegeEscalation: &no,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "mail", MountPath: MailRoot}},
	}
}

func mailVolume(claim string) corev1.Volume {
	return corev1.Volume{Name: "mail", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
	}}
}

// MailboxBackupJob copies every mailbox of one mail domain into the bundle.
//
// claim is the mail server's volume; p.Node the node the server holds it
// on. INDEX in the archive names the mailboxes, one per line, and <line
// number> is each one's mail.
//
// A copy is checked before it counts: doveadm reports some failures and
// exits 0, so the script counts the messages of the mailbox and of its copy
// and fails when they differ. Mail keeps arriving while this runs, so a
// difference is tried again before it is believed.
//
// A domain that has no directory has no mailboxes: the archive is written
// with an empty INDEX, the record that it was looked at.
//
// The mailboxes of removed people that were archived (mailbox_person.go) are
// copied the same way, into archived/ of the same archive with an INDEX of
// their own: they are in the bundle as what they are.
func MailboxBackupJob(p JobParams, claim, domain string) *batchv1.Job {
	script := doveadmPrelude + fmt.Sprintf(`# copy_boxes DIRECTORY INTO WHAT: every mailbox below DIRECTORY, copied to
# INTO/<number> and named, one per line, in INTO/INDEX. copied is how many.
copy_boxes() {
  _from="$1"; _into="$2"; _what="$3"
  mkdir -p "${_into}"
  : > "${_into}/INDEX"
  copied=0
  [ -d "${_from}" ] || return 0
  # A name is whatever an address was: listed one per line it must not hold
  # a line break, and a mailbox that cannot be listed is not left out.
  find "${_from}" -mindepth 1 -maxdepth 1 -name '*[[:cntrl:]]*' > /tmp/unlistable
  if [ -s /tmp/unlistable ]; then
    echo "ERROR: ${_what} of ${DOMAIN} has a control character in its name and cannot be carried" >&2; exit 1
  fi
  find "${_from}" -mindepth 1 -maxdepth 1 -type d -printf '%%f\n' > /tmp/unsorted
  LC_ALL=C sort /tmp/unsorted > /tmp/boxes
  while IFS= read -r box; do
    [ -n "${box}" ] || continue
    src="${_from}/${box}"
    dst="${_into}/${copied}"
    tries=0
    while : ; do
      rm -rf "${dst}"
      dv "${src}" backup "maildir:${dst}"
      have="$(count "${src}")"
      got="$(count "${dst}")"
      [ "${have}" != "${got}" ] || break
      tries=$((tries + 1))
      if [ "${tries}" -ge 3 ]; then
        echo "ERROR: the copy of ${_what} ${box} of ${DOMAIN} holds ${got} message(s) and the original ${have}" >&2; exit 1
      fi
    done
    printf '%%s\n' "${box}" >> "${_into}/INDEX"
    echo "copied ${_what} ${box} of ${DOMAIN}: ${got} message(s)"
    copied=$((copied + 1))
  done < /tmp/boxes
}
copy_boxes "%[1]s/${DOMAIN}" "%[2]s/mail" "the mailbox"
echo "copied ${copied} mailbox(es) of ${DOMAIN}"
# The mailboxes of people who were removed and whose mail was archived: as
# archives, beside the live ones and never among them.
copy_boxes "%[1]s/%[3]s/${DOMAIN}" "%[2]s/mail/%[4]s" "the archived mailbox"
echo "copied ${copied} archived mailbox(es) of ${DOMAIN}"`, MailRoot, workDir, MailArchiveDir, archivedInBundle)
	copyBoxes := mailContainer("mailbox-backup", script, corev1.EnvVar{Name: "DOMAIN", Value: domain})
	copyBoxes.VolumeMounts = append(copyBoxes.VolumeMounts, corev1.VolumeMount{Name: "work", MountPath: workDir})
	// A separate container, as for every archive: one image packs them all.
	pack := corev1.Container{
		Name:    "pack-mailboxes",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
tar czf %[1]s/mailboxes.tar.gz -C %[1]s/mail .
rm -rf %[1]s/mail
echo "archived the mailboxes"`, workDir)},
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	return uploadJob(p, "mailboxes.tar.gz", MailboxesArtefact, []corev1.Container{copyBoxes, pack}, []corev1.Volume{mailVolume(claim)})
}

// MailboxRestoreJob puts the mailboxes of a bundle back, into the mailboxes
// of one mail domain: each under the name it had, which is the part of its
// address before the @, whatever the domain was where the bundle was taken.
//
// On top, as files are: a message the bundle holds and the mailbox lacks is
// added, with its flags and in its folder, and a message that arrived since
// the bundle was taken stays. Mail goes on arriving while a tenant is being
// restored; a restore that made each mailbox what the bundle holds would
// delete it.
//
// The archive comes from a bundle, and a bundle can come from anywhere. A
// name in its INDEX is used as a directory's and is refused unless it is
// one; a link in the archive is refused, because the synchronisation would
// follow it, and what it reads it writes into the tenant's mailbox.
func MailboxRestoreJob(p JobParams, d Decryption, artefact, claim, domain string) *batchv1.Job {
	unpack := corev1.Container{
		Name:    "unpack-mailboxes",
		Image:   kernel.KeycloakProvisionerImage(),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{fmt.Sprintf(`set -eu
mkdir -p %[1]s/mail
tar xzf %[1]s/mailboxes.tar.gz -C %[1]s/mail
rm -f %[1]s/mailboxes.tar.gz
[ -f %[1]s/mail/INDEX ] || { echo "ERROR: the archive has no INDEX" >&2; exit 1; }
find %[1]s/mail ! -type f ! -type d > /tmp/odd
if [ -s /tmp/odd ]; then
  echo "ERROR: refused: the archive holds something that is neither a file nor a directory: $(head -n 3 /tmp/odd)" >&2; exit 1
fi
# The mail server's user reads and indexes them.
chmod -R a+rwX %[1]s/mail
echo "unpacked the archive of mailboxes"`, workDir)},
		VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: workDir}},
	}
	script := doveadmPrelude + fmt.Sprintf(`# put_back FROM INTO WHAT: every mailbox FROM/INDEX names, synchronised into
# the directory of its name below INTO. restored is how many.
put_back() {
  _from="$1"; _into="$2"; _what="$3"
  restored=0
  mkdir -p "${_into}"
  while IFS= read -r box; do
    [ -n "${box}" ] || continue
    case "${box}" in
      .|..|*/*|*[[:cntrl:]]*) echo "ERROR: refused: the archive names ${_what} ${box}, which is not a name a mailbox can have" >&2; exit 1 ;;
    esac
    src="${_from}/${restored}"
    [ -d "${src}" ] || { echo "ERROR: the archive lists ${_what} ${box} and holds no mail of it" >&2; exit 1; }
    dst="${_into}/${box}"
    # One way, from the bundle into the mailbox: nothing the mailbox holds is
    # removed.
    dv "${dst}" sync -1 -R "maildir:${src}"
    want="$(count "${src}")"
    got="$(count "${dst}")"
    if [ "${got}" -lt "${want}" ]; then
      echo "ERROR: ${_what} ${box} of ${DOMAIN} holds ${got} message(s) after the restore and the bundle ${want}" >&2; exit 1
    fi
    echo "restored ${_what} ${box} of ${DOMAIN}: ${want} message(s) of the bundle, ${got} in the mailbox"
    restored=$((restored + 1))
  done < "${_from}/INDEX"
}
put_back "%[2]s/mail" "%[1]s/${DOMAIN}" "the mailbox"
echo "restored ${restored} mailbox(es) into ${DOMAIN}"
# What the bundle holds as archived comes back as archived: below the
# archive, where no address opens it, and never into a live mailbox. A bundle
# taken before mailboxes were archived holds none.
if [ -f "%[2]s/mail/%[4]s/INDEX" ]; then
  for link in "%[1]s/%[3]s" "%[1]s/%[3]s/${DOMAIN}"; do
    if [ -L "${link}" ]; then echo "ERROR: refused: ${link} is a link" >&2; exit 1; fi
  done
  put_back "%[2]s/mail/%[4]s" "%[1]s/%[3]s/${DOMAIN}" "the archived mailbox"
  echo "restored ${restored} archived mailbox(es) of ${DOMAIN}"
fi`, MailRoot, workDir, MailArchiveDir, archivedInBundle)
	restore := mailContainer("mailbox-restore", script, corev1.EnvVar{Name: "DOMAIN", Value: domain})
	restore.VolumeMounts = append(restore.VolumeMounts, corev1.VolumeMount{Name: "work", MountPath: workDir})
	return restoreJob(p, []corev1.Container{
		fetchAndDecrypt(d, p, artefact, "mailboxes.tar.gz"),
		unpack,
	}, restore, []corev1.Volume{mailVolume(claim)})
}

// mailboxDestroyScript removes every mailbox of one mail domain, the
// domain's directory, and the archived mailboxes of the domain's removed
// people.
//
// It ends in success only when the directory is verifiably not there, by
// the rules the stores' destroy scripts go by: no command's failure is
// discarded, and "gone" is what the volume's own listing says afterwards: a
// volume that cannot be listed fails the Job.
func mailboxDestroyScript() string {
	return fmt.Sprintf(`set -eu
ROOT=%[1]s
case "${DOMAIN}" in
  ""|.*|*/*) echo "ERROR: refused: ${DOMAIN} is not a mail domain's directory" >&2; exit 1 ;;
esac
ls -A "${ROOT}" >/dev/null
if [ -e "${ROOT}/${DOMAIN}" ]; then
  rm -rf -- "${ROOT:?}/${DOMAIN:?}"
  echo "the mailboxes of ${DOMAIN} are removed"
else
  echo "${DOMAIN} has no mailboxes"
fi
listing="$(ls -A "${ROOT}")"
if printf '%%s\n' "${listing}" | grep -qxF -- "${DOMAIN}"; then
  echo "ERROR: the mailboxes of ${DOMAIN} are still there" >&2; exit 1
fi
echo "the mailboxes of ${DOMAIN} are gone"
# The archived mailboxes of the domain's removed people go with them.
ARCHIVE="${ROOT}/%[2]s"
if [ -L "${ARCHIVE}" ]; then echo "ERROR: refused: ${ARCHIVE} is a link" >&2; exit 1; fi
if [ -e "${ARCHIVE}/${DOMAIN}" ]; then
  rm -rf -- "${ARCHIVE:?}/${DOMAIN:?}"
  echo "the archived mailboxes of ${DOMAIN} are removed"
fi
if [ -e "${ARCHIVE}" ]; then
  listing="$(ls -A "${ARCHIVE}")"
  if printf '%%s\n' "${listing}" | grep -qxF -- "${DOMAIN}"; then
    echo "ERROR: the archived mailboxes of ${DOMAIN} are still there" >&2; exit 1
  fi
fi
echo "the archived mailboxes of ${DOMAIN} are gone"
`, MailRoot, MailArchiveDir)
}

// MailboxDestroyJob destroys every mailbox of one mail domain: the Job a
// tenant's deletion runs. It is shaped like the stores' destroy Jobs --
// labelled so the deletion finds it, bounded, failing rather than retrying
// for ever -- and runs where the mail server's volume is, on the node the
// server holds it on.
func MailboxDestroyJob(namespace, tenantName, claim, node, domain string, deadline DestroyDeadline) *batchv1.Job {
	container := mailContainer("delete-mailboxes", mailboxDestroyScript(), corev1.EnvVar{Name: "DOMAIN", Value: domain})
	container.Command = []string{"/bin/sh", "-c"}
	context := container.SecurityContext
	job := destroyJob(namespace, MailboxDestroyJobName(tenantName), tenantName, mailStore, deadline, container)
	spec := &job.Spec.Template.Spec
	// destroyJob gives its container the restricted context; this one also
	// says who it runs as.
	spec.Containers[0].SecurityContext = context
	spec.Volumes = []corev1.Volume{mailVolume(claim)}
	spec.NodeSelector = nodeSelectorFor(node)
	return job
}

// mailStore is the name the mailbox Jobs are labelled with, in the place of
// an app's: like the desktop's and the backup bucket's it is shaped like an
// app's name and is none.
const mailStore = "gentian-mail"
