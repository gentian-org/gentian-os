#!/usr/bin/env bash
# Round-trip the recovery kit: export one, load it back, and prove every value
# returns byte-exact.
#
# The kit is what rebuilds a cluster after its OpenBao is gone. Until now only
# its export was ever exercised — a kit was written and inspected, and nothing
# had read one back. That leaves the half that matters unproven: a kit that
# writes cleanly and reloads wrong is worse than no kit at all, because it is
# discovered at the one moment there is nothing to fall back on.
#
# DERIVATION_SALT is the value this test exists for. Every derived credential in
# the cluster is HKDF'd from it, so a salt that survives the round trip altered
# by even one byte reproduces an entire cluster's worth of wrong passwords —
# and the symptom is an admin login that fails for no visible reason.
#
# Runs without a cluster: values come from the environment, which _kit_gather
# prefers over OpenBao and kubectl.
#
# shellcheck disable=SC2016  # single quotes are the point: the child shells
# must expand SCRIPT_DIR themselves, and the test values must contain a literal
# $(...) to prove a kit carries shell metacharacters without executing them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT_DIR="${ROOT}"
export SCRIPT_DIR

if ! command -v age >/dev/null 2>&1; then
    echo "SKIP: age is not installed."
    echo "  The unattended kit path (recipient encryption) needs it, and the openssl"
    echo "  fallback reads its passphrase from a terminal, so neither can be driven"
    echo "  from a test without one. Install age to run this check."
    exit 0
fi

# shellcheck source=scripts/lib/load.sh
source "${ROOT}/scripts/lib/load.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

failures=0
check() {
    local what="$1" want="$2" got="$3"
    if [[ "${got}" == "${want}" ]]; then
        printf '  [OK]       %s\n' "${what}"
    else
        printf '  [MISMATCH] %s\n' "${what}"
        printf '             want: %q\n' "${want}"
        printf '             got:  %q\n' "${got}"
        failures=$((failures + 1))
    fi
}

# An ephemeral recipient, so the test never touches a real kit key.
age-keygen -o "${WORK}/identity.txt" 2>/dev/null
GENTIAN_KIT_RECIPIENT="$(age-keygen -y "${WORK}/identity.txt")"
GENTIAN_KIT_IDENTITY="${WORK}/identity.txt"
export GENTIAN_KIT_RECIPIENT GENTIAN_KIT_IDENTITY

# Deliberately hostile values. The kit base64-encodes each one precisely so
# that newlines, quotes, backslashes, shell metacharacters and non-ASCII
# survive; a value that only ever contained [A-Za-z0-9] would prove nothing.
WANT_MASTER_PASSWORD='p@ss "with" $(quotes) `and` \backslashes\ and ümlauts'
WANT_DERIVATION_SALT=$'salt-with\nnewline\tand-tab'
WANT_TOKEN="ghp_$(printf 'x%.0s' {1..36})"
WANT_DOMAIN='desk.gentian.org'

export MASTER_PASSWORD="${WANT_MASTER_PASSWORD}"
export DERIVATION_SALT="${WANT_DERIVATION_SALT}"
export GENTIAN_DEPLOYMENTS_GIT_TOKEN="${WANT_TOKEN}"
export KERNEL_DOMAIN="${WANT_DOMAIN}"
# Set so _kit_gather never reaches for kubectl or OpenBao during the test.
export TRANSIT_UNSEAL_KEY="transit-key" OPENBAO_RECOVERY_KEYS="k1,k2" OPENBAO_ROOT_TOKEN="root-token"

KIT="${WORK}/kit.age"

echo "Export"
if ! export_recovery_kit "${KIT}" >"${WORK}/export.log" 2>&1; then
    echo "  [FAIL] export_recovery_kit returned non-zero:"
    sed 's/^/         /' "${WORK}/export.log"
    exit 1
fi
[[ -s "${KIT}" ]] || { echo "  [FAIL] no kit was written"; exit 1; }
perms="$(stat -c '%a' "${KIT}" 2>/dev/null || stat -f '%Lp' "${KIT}")"
check "kit is mode 0600" "600" "${perms}"
# Anyone who can read the file must not be able to read the credentials in it.
if grep -qaF "${WANT_TOKEN}" "${KIT}"; then
    echo "  [FAIL] the git token appears in the kit as plaintext"
    failures=$((failures + 1))
else
    printf '  [OK]       kit is encrypted (no plaintext credential found)\n'
fi

echo "Load"
# A clean shell: the kit must supply the values, not inherit them. Sourcing in
# the same process would let the exporting environment mask a lost value.
#
# The child writes NUL-delimited records to a file rather than to a command
# substitution, which strips NUL bytes — and a NUL delimiter is the only one a
# value containing newlines cannot forge.
env -u MASTER_PASSWORD -u DERIVATION_SALT -u GENTIAN_DEPLOYMENTS_GIT_TOKEN -u KERNEL_DOMAIN \
    SCRIPT_DIR="${ROOT}" \
    GENTIAN_KIT_IDENTITY="${GENTIAN_KIT_IDENTITY}" \
    bash -c '
        source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
        load_recovery_kit "'"${KIT}"'" >/dev/null 2>&1 || exit 1
        printf "MASTER_PASSWORD=%s\0" "${MASTER_PASSWORD:-}"
        printf "DERIVATION_SALT=%s\0" "${DERIVATION_SALT:-}"
        printf "TOKEN=%s\0" "${GENTIAN_DEPLOYMENTS_GIT_TOKEN:-}"
        printf "DOMAIN=%s\0" "${KERNEL_DOMAIN:-}"
    ' > "${WORK}/readback.bin" || { echo "  [FAIL] load_recovery_kit returned non-zero"; exit 1; }

readval() {
    local key="$1" rec
    while IFS= read -r -d '' rec; do
        [[ "${rec}" == "${key}="* ]] && { printf '%s' "${rec#"${key}="}"; return; }
    done < "${WORK}/readback.bin"
}

check "MASTER_PASSWORD survives quotes, metacharacters and unicode" \
    "${WANT_MASTER_PASSWORD}" "$(readval MASTER_PASSWORD)"
check "DERIVATION_SALT survives newlines and tabs" \
    "${WANT_DERIVATION_SALT}" "$(readval DERIVATION_SALT)"
check "the deployments token survives" "${WANT_TOKEN}" "$(readval TOKEN)"
check "KERNEL_DOMAIN survives" "${WANT_DOMAIN}" "$(readval DOMAIN)"

echo "Rejections"
# A kit is encrypted, which is not the same as trusted: load_recovery_kit
# whitelists key names rather than sourcing the file, so a key that could set
# PATH or BAO_ADDR is ignored instead of obeyed.
printf '# gentian-os recovery kit\nGENTIAN_KIT_VERSION=1\nPATH=%s\nMASTER_PASSWORD=%s\n' \
    "$(printf '/evil' | openssl base64 -A)" \
    "$(printf 'from-kit' | openssl base64 -A)" \
    | age -r "${GENTIAN_KIT_RECIPIENT}" -o "${WORK}/hostile.age"
hostile_path="$(
    env SCRIPT_DIR="${ROOT}" GENTIAN_KIT_IDENTITY="${GENTIAN_KIT_IDENTITY}" bash -c '
        source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
        load_recovery_kit "'"${WORK}/hostile.age"'" >/dev/null 2>&1
        printf "%s" "${PATH}"
    '
)"
if [[ "${hostile_path}" == "/evil" ]]; then
    echo "  [FAIL] a key in the kit overwrote PATH"
    failures=$((failures + 1))
else
    printf '  [OK]       unknown keys are ignored, not applied\n'
fi

# Not a kit at all: refused rather than partially applied.
printf 'this is not a kit\n' | age -r "${GENTIAN_KIT_RECIPIENT}" -o "${WORK}/garbage.age"
if env SCRIPT_DIR="${ROOT}" GENTIAN_KIT_IDENTITY="${GENTIAN_KIT_IDENTITY}" bash -c '
        source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
        load_recovery_kit "'"${WORK}/garbage.age"'"' >/dev/null 2>&1; then
    echo "  [FAIL] a file that is not a kit was accepted"
    failures=$((failures + 1))
else
    printf '  [OK]       a file that is not a kit is refused\n'
fi

# A kit that cannot be decrypted must fail, not yield an empty environment that
# looks like a successful load of nothing.
if env SCRIPT_DIR="${ROOT}" -u GENTIAN_KIT_IDENTITY bash -c '
        source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
        load_recovery_kit "'"${KIT}"'" </dev/null' >/dev/null 2>&1; then
    echo "  [FAIL] a kit was 'loaded' without the identity that encrypted it"
    failures=$((failures + 1))
else
    printf '  [OK]       a kit without its identity is refused\n'
fi

echo ""
if [[ ${failures} -eq 0 ]]; then
    echo "Recovery kit round trip verified."
    exit 0
fi
echo "${failures} problem(s) — a kit that does not round trip cannot rebuild a cluster."
exit 1
