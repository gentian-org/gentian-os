#!/bin/sh
# A daily snapshot of one of the platform's two OpenBao instances
# (docs/guides/recovery.md §4), run by the CronJobs of
# charts/gentian-os/templates/openbao-snapshots.yaml: the vault
# (openbao-snapshot, in the vault's namespace) and the seal that unseals it
# (openbao-transit-snapshot, in the seal's namespace). One script, two
# instances, told apart by the environment.
#
# It asks the instance which storage it is on; on anything but Raft it says
# so and takes nothing, because only Raft has a consistent online snapshot.
# On Raft it logs in with its own ServiceAccount's token (a role that may
# read sys/storage/raft/snapshot and nothing else), streams the snapshot
# through age to the backup recipient, and uploads the ciphertext to the
# platform's object store. The plaintext never touches a disk and never
# leaves the pod. Then it keeps the newest KEEP snapshots and deletes the
# rest. Either way it leaves an empty object storage.<raft|file> beside
# them, which is how the operator learns what storage the instance is on
# without reaching it.
#
# The recipient is the one the cluster's values pin in git
# (backupRecipients, BACKUP_AGE_RECIPIENTS), else the one the recovery kit
# wrote to the vault (gentian-os/kernel/backup/recipients, mounted at
# RECIPIENTS_FILE): the order the operator reads them in
# (internal/controller/tenantexport_encryption.go). With neither it refuses:
# an unencrypted copy of the vault is worse than none.
#
# No secret is ever an argument of a command: the login body is built from
# the token file by jq, and the token goes to curl as a header file.
#
# Environment:
#   BAO_ADDR                   the instance
#   BAO_CACERT                 the CA it is verified with; empty for plain HTTP
#   BAO_AUTH_PATH, BAO_ROLE    the Kubernetes auth mount and the role
#   JWT_FILE                   the projected ServiceAccount token
#   S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY, S3_REGION
#   BACKUP_AGE_RECIPIENTS      age1... recipients, comma separated; or
#   RECIPIENTS_FILE            a file holding them
#   KEEP                       how many snapshots to keep (at least 1)
#   SNAPSHOT_NAME              the instance, and the stem of its objects (openbao)
#   SNAPSHOT_PREFIX            the key prefix in the bucket (the name)
#   WORK_DIR                   where the ciphertext is staged
set -eu
# No pipe whose first command can fail unseen: the one pipe, the snapshot
# into age, writes its first command's exit status to a file that is read
# after it, so this runs alike under any POSIX sh, with or without pipefail.

name="${SNAPSHOT_NAME:-openbao}"
log() { printf '%s %s-snapshot: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${name}" "$*"; }
fail() { log "ERROR: $*" >&2; exit 1; }

work="${WORK_DIR:-/work}"
keep="${KEEP:-14}"
prefix="${SNAPSHOT_PREFIX:-${name}}"
case "${keep}" in
    ''|*[!0-9]*) fail "KEEP is \"${keep}\"; it is a whole number" ;;
esac
[ "${keep}" -ge 1 ] || fail "KEEP is ${keep}; at least the newest snapshot is kept"
case "${name}" in
    ''|*[!a-z0-9-]*) fail "SNAPSHOT_NAME is \"${name}\"; lower case letters, digits and dashes" ;;
esac

umask 077
mkdir -p "${work}"
header="${work}/token-header"
login="${work}/login.json"
read_status="${work}/snapshot.status"
marker="${work}/storage-marker"
# Whatever happens, the token does not outlive the run.
trap 'rm -f "${header}" "${login}" "${read_status}" "${marker}"' EXIT

bao() {
    if [ -n "${BAO_CACERT:-}" ]; then
        curl -sS --fail --max-time "${CURL_MAX_TIME:-600}" --cacert "${BAO_CACERT}" "$@"
    else
        curl -sS --fail --max-time "${CURL_MAX_TIME:-600}" "$@"
    fi
}

have_store() {
    [ -n "${S3_ENDPOINT:-}" ] && [ -n "${S3_BUCKET:-}" ] &&
        [ -n "${S3_ACCESS_KEY:-}" ] && [ -n "${S3_SECRET_KEY:-}" ]
}

# rclone's remote from the environment (internal/backup/objectstore.go), never
# from a command line. RCLONE_S3_NO_CHECK_BUCKET: the key may write objects in
# its bucket and may not make one.
remote_from_env() {
    export RCLONE_CONFIG_SNAP_TYPE=s3 RCLONE_CONFIG_SNAP_PROVIDER=Other RCLONE_S3_NO_CHECK_BUCKET=true
    export RCLONE_CONFIG_SNAP_ENDPOINT="${S3_ENDPOINT}" RCLONE_CONFIG_SNAP_ACCESS_KEY_ID="${S3_ACCESS_KEY}"
    export RCLONE_CONFIG_SNAP_SECRET_ACCESS_KEY="${S3_SECRET_KEY}" RCLONE_CONFIG_SNAP_FORCE_PATH_STYLE=true
    if [ -n "${S3_REGION:-}" ]; then export RCLONE_CONFIG_SNAP_REGION="${S3_REGION}"; fi
    remote="snap:${S3_BUCKET}/${prefix}"
}

# mark_storage <type> — the empty object storage.<type>, and not the other.
mark_storage() {
    : > "${marker}"
    rclone copyto "${marker}" "${remote}/storage.$1" ||
        { log "the storage marker ${prefix}/storage.$1 could not be written"; return 0; }
    for other in raft file; do
        [ "${other}" = "$1" ] && continue
        rclone deletefile "${remote}/storage.${other}" >/dev/null 2>&1 || true
    done
}

# ── Which storage ────────────────────────────────────────────────────────────
status="$(bao "${BAO_ADDR}/v1/sys/seal-status")" ||
    fail "${name} at ${BAO_ADDR} did not answer sys/seal-status"
storage="$(printf '%s' "${status}" | jq -r '.storage_type // ""')"
case "${storage}" in
    raft) ;;
    '') fail "${name} did not say which storage it is on (sys/seal-status has no storage_type)" ;;
    *)
        log "NOT ON RAFT: ${name} keeps its data on ${storage} storage, which has no consistent online snapshot."
        log "No snapshot taken. Moving it to Raft is docs/guides/recovery.md §5."
        if have_store; then
            remote_from_env
            mark_storage "${storage}"
        fi
        exit 0 ;;
esac
[ "$(printf '%s' "${status}" | jq -r '.sealed')" = false ] ||
    fail "${name} is sealed; a sealed instance serves no snapshot"

# ── To whom ──────────────────────────────────────────────────────────────────
recipients="${BACKUP_AGE_RECIPIENTS:-}"
if [ -z "${recipients}" ] && [ -n "${RECIPIENTS_FILE:-}" ] && [ -r "${RECIPIENTS_FILE}" ]; then
    recipients="$(cat "${RECIPIENTS_FILE}")"
fi
recipients_file="${work}/recipients.txt"
: > "${recipients_file}"
for r in $(printf '%s' "${recipients}" | tr ',' ' '); do
    case "${r}" in
        age1*) printf '%s\n' "${r}" >> "${recipients_file}" ;;
        *) fail "\"${r}\" is not an age recipient (age1...)" ;;
    esac
done
[ -s "${recipients_file}" ] ||
    fail "no backup recipient: neither backupRecipients in this cluster's values nor gentian-os/kernel/backup/recipients in the vault names one. Refusing to write an unencrypted snapshot."

# ── Where to ─────────────────────────────────────────────────────────────────
have_store ||
    fail "the operator has not made the snapshot store yet: the Secret this job reads from gentian-os/kernel/openbao-snapshots/${name} is missing or incomplete"
remote_from_env

# ── Log in ───────────────────────────────────────────────────────────────────
[ -r "${JWT_FILE}" ] || fail "no ServiceAccount token at ${JWT_FILE}"
jq -n --arg role "${BAO_ROLE}" --rawfile jwt "${JWT_FILE}" '{role: $role, jwt: $jwt}' > "${login}" ||
    fail "the login request could not be written"
answer="$(bao -X POST --data "@${login}" "${BAO_ADDR}/v1/auth/${BAO_AUTH_PATH}/login")" ||
    fail "${name} refused the login as role ${BAO_ROLE} on auth/${BAO_AUTH_PATH}"
rm -f "${login}"
token="$(printf '%s' "${answer}" | jq -r '.auth.client_token // ""')"
unset answer
[ -n "${token}" ] || fail "${name} answered the login with no token"
printf 'X-Vault-Token: %s\n' "${token}" > "${header}"
unset token

# ── Snapshot, encrypted on the way ───────────────────────────────────────────
# curl writes the snapshot into a pipe and age reads it from there: the
# plaintext is never in a file. curl's exit status goes to a file beside, so
# a snapshot cut short -- or never begun, when curl fails before it writes a
# byte, which closes the pipe and ends age at once -- fails the run however
# much age had encrypted.
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
object="${name}-${stamp}.snap.age"
staged="${work}/${object}"
rm -f "${read_status}"
age_rc=0
{
    rc=0
    bao -H "@${header}" "${BAO_ADDR}/v1/sys/storage/raft/snapshot" || rc=$?
    echo "${rc}" > "${read_status}"
} | age -R "${recipients_file}" -o "${staged}" || age_rc=$?
read_rc="$(cat "${read_status}" 2>/dev/null || echo unknown)"
rm -f "${header}" "${read_status}"
if [ "${read_rc}" != 0 ] || [ "${age_rc}" -ne 0 ]; then
    rm -f "${staged}"
    fail "the snapshot could not be read (curl ${read_rc}) or encrypted (age ${age_rc})"
fi
size="$(wc -c < "${staged}" | tr -d ' ')"
# An age file of an empty input is about 200 bytes; a snapshot of an
# instance that holds anything is far more.
[ "${size}" -gt 1024 ] || fail "the encrypted snapshot is ${size} bytes; ${name} answered with no snapshot"

# ── Upload ───────────────────────────────────────────────────────────────────
rclone copyto "${staged}" "${remote}/${object}" ||
    fail "the snapshot could not be written to ${S3_BUCKET}/${prefix}/${object}"
rm -f "${staged}"
log "snapshot ${S3_BUCKET}/${prefix}/${object} written, ${size} bytes, encrypted to $(wc -l < "${recipients_file}" | tr -d ' ') recipient(s)"
mark_storage raft

# ── Retention ────────────────────────────────────────────────────────────────
# The names sort by time. Only after the new one is written, and never the
# newest KEEP.
listing="$(rclone lsf --files-only "${remote}/")" ||
    fail "the snapshots in ${S3_BUCKET}/${prefix} could not be listed; none was deleted"
old="$(printf '%s\n' "${listing}" | grep -E "^${name}-[0-9]{8}T[0-9]{6}Z\\.snap\\.age\$" | sort | head -n "-${keep}" || true)"
for o in ${old}; do
    rclone deletefile "${remote}/${o}" || fail "the old snapshot ${o} could not be deleted"
    log "deleted ${S3_BUCKET}/${prefix}/${o}, beyond the newest ${keep}"
done
log "done"
