#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-teardown-lock.sh
# =============================================================================
# What install.sh runs before an uninstall or a purge, teardown_start, held
# against the two ways it can go wrong:
#
#   The teardown lock and the port-forward cleanup share the EXIT trap through
#   gentian_on_exit: after a purge is confirmed, at the prompt or by
#   GENTIAN_PURGE_CONFIRM, exiting still kills the port-forwards this run
#   started, and removes the lock.
#   While one teardown holds the lock, a second is refused, whether it is a
#   purge confirmed by GENTIAN_PURGE_CONFIRM, one confirmed at the prompt, or an
#   uninstall. A lock left by a process that is gone is taken over.
#
# Only the sourced library runs here, never install.sh, and nothing reaches a
# cluster: the "port-forward" is a sleep whose pid is in the forward list.
# =============================================================================
# The scripts handed to run() are for the child shell to expand:
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
mkdir -p "${SANDBOX}/tmp" "${SANDBOX}/home"
CLUSTER=lab
LOCK="${SANDBOX}/tmp/gentian-teardown-${CLUSTER}.lock"

# run <script> [VAR=value ...] — the installer's libraries, as install.sh loads
# them, in a shell of their own whose exit runs the EXIT trap.
run() {
    local script="$1"; shift
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" TMPDIR="${SANDBOX}/tmp" \
        SB="${SANDBOX}" GENTIAN_DEPLOYMENTS_CLUSTER_ID="${CLUSTER}" SCRIPT="${script}" "$@" \
        bash -c '
            source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
            trap - ERR; set +e
            eval "${SCRIPT}"
        ' 2>&1
}

# gone <pid> — true once the process has exited, within five seconds.
gone() {
    local _
    for _ in 1 2 3 4 5 6 7 8 9 10; do
        kill -0 "$1" 2>/dev/null || return 0
        sleep 0.5
    done
    return 1
}

# A port-forward this run started, then the confirmation, then exit.
START_WITH_FORWARD='
    sleep 300 &
    echo "$!" > "${SB}/forward.pid"
    echo "$!" >> "${_GENTIAN_PF_PIDFILE}"
    echo "${_GENTIAN_PF_PIDFILE}" > "${SB}/pidfile"
    teardown_start; echo "rc=$?"
    [[ -f "${TMPDIR}/gentian-teardown-${GENTIAN_DEPLOYMENTS_CLUSTER_ID}.lock" ]] && echo "locked"
'

check_cleanup() {
    local label="$1" out="$2" pid pidfile
    pid="$(cat "${SANDBOX}/forward.pid" 2>/dev/null)"
    pidfile="$(cat "${SANDBOX}/pidfile" 2>/dev/null)"
    if [[ "${out}" != *"rc=0"* || "${out}" != *"locked"* ]]; then
        bad "${label}: the teardown did not start with the lock taken" "${out}"
    elif [[ -z "${pid}" ]] || ! gone "${pid}"; then
        kill "${pid}" 2>/dev/null
        bad "${label}: the port-forward outlived the run" "${out}"
    elif [[ -n "${pidfile}" && -e "${pidfile}" ]]; then
        bad "${label}: the port-forward list was left behind (${pidfile})"
    elif [[ -e "${LOCK}" ]]; then
        bad "${label}: the teardown lock was left behind"
    else
        ok "${label}: exiting kills the port-forward and removes the lock"
    fi
    rm -f "${SANDBOX}/forward.pid" "${SANDBOX}/pidfile"
}

echo "Teardown: the port-forward cleanup runs after the confirmation"

o="$(run "${START_WITH_FORWARD}" GENTIAN_PURGE=1 GENTIAN_PURGE_CONFIRM="${CLUSTER}")"
check_cleanup "purge confirmed by GENTIAN_PURGE_CONFIRM" "${o}"

o="$(printf '%s\n' "${CLUSTER}" | run "${START_WITH_FORWARD}" GENTIAN_PURGE=1)"
check_cleanup "purge confirmed at the prompt" "${o}"

o="$(run "${START_WITH_FORWARD}")"
check_cleanup "uninstall" "${o}"

o="$(run 'teardown_start; echo "rc=$?"' GENTIAN_PURGE=1 GENTIAN_PURGE_CONFIRM=other)"
if [[ "${o}" == *"rc=1"* && "${o}" == *"does not match"* && ! -e "${LOCK}" ]]; then
    ok "a GENTIAN_PURGE_CONFIRM naming another cluster is refused, and the lock released"
else
    bad "a GENTIAN_PURGE_CONFIRM naming another cluster was not refused cleanly" "${o}"
fi

echo "Teardown: a second one is refused while the lock is held"

# The first purge: takes the lock, then waits until told to finish.
run 'teardown_start || exit 1; echo "$$" > "${SB}/holder"
     while [[ ! -e "${SB}/release" ]]; do sleep 0.2; done' \
    GENTIAN_PURGE=1 GENTIAN_PURGE_CONFIRM="${CLUSTER}" > "${SANDBOX}/holder.log" &
HOLDER_JOB=$!
for _ in $(seq 1 50); do [[ -s "${SANDBOX}/holder" ]] && break; sleep 0.1; done
HOLDER="$(cat "${SANDBOX}/holder" 2>/dev/null)"

if [[ -z "${HOLDER}" ]]; then
    bad "the first purge never took the lock" "$(cat "${SANDBOX}/holder.log")"
else
    second() {
        local label="$1"; shift
        local o
        o="$(run 'teardown_start; echo "rc=$?"' "$@" < /dev/null)"
        if [[ "${o}" == *"rc=1"* && "${o}" == *"already running"*"pid ${HOLDER}"* \
              && "$(cat "${LOCK}" 2>/dev/null)" == "${HOLDER}" ]]; then
            ok "${label} is refused, and the first purge keeps the lock"
        else
            bad "${label} was not refused while the lock is held" "${o}"
        fi
    }
    second "a purge confirmed by GENTIAN_PURGE_CONFIRM" GENTIAN_PURGE=1 GENTIAN_PURGE_CONFIRM="${CLUSTER}"
    o="$(printf '%s\n' "${CLUSTER}" | run 'teardown_start; echo "rc=$?"' GENTIAN_PURGE=1)"
    if [[ "${o}" == *"rc=1"* && "${o}" == *"already running"* && "${o}" != *"PURGE removes"* ]]; then
        ok "a purge confirmed at the prompt is refused before it prompts"
    else
        bad "a purge confirmed at the prompt was not refused before the prompt" "${o}"
    fi
    second "an uninstall"
fi

touch "${SANDBOX}/release"
wait "${HOLDER_JOB}" 2>/dev/null
rm -f "${SANDBOX}/release"
if [[ -e "${LOCK}" ]]; then
    bad "the first purge left its lock behind when it finished"
else
    o="$(run 'teardown_start; echo "rc=$?"' GENTIAN_PURGE=1 GENTIAN_PURGE_CONFIRM="${CLUSTER}")"
    if [[ "${o}" == *"rc=0"* ]]; then
        ok "once the first purge has finished, the next one starts"
    else
        bad "the next purge did not start after the first finished" "${o}"
    fi
fi

# A lock whose holder is gone: a pid that has exited.
sleep 0 & DEAD=$!; wait "${DEAD}"
echo "${DEAD}" > "${LOCK}"
o="$(run 'teardown_start; echo "rc=$?"; cat "${TMPDIR}/gentian-teardown-${GENTIAN_DEPLOYMENTS_CLUSTER_ID}.lock"; echo "self=$$"' \
    GENTIAN_PURGE=1 GENTIAN_PURGE_CONFIRM="${CLUSTER}")"
self="$(printf '%s\n' "${o}" | sed -n 's/^self=//p')"
if [[ "${o}" == *"rc=0"* && "${o}" == *"Stale teardown lock"* && -n "${self}" \
      && "$(printf '%s\n' "${o}" | grep -cx "${self}")" == "1" && ! -e "${LOCK}" ]]; then
    ok "a lock left by a process that is gone is taken over, and removed on exit"
else
    bad "a stale lock was not taken over" "${o}"
fi

finish
