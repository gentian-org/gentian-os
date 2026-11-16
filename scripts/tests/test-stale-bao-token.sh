#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-stale-bao-token.sh
# =============================================================================
# A BAO_TOKEN left exported from an earlier session has usually expired. The
# installer used to take it unasked, and B-10's check then read the director's
# key through it, got a 403, called the key missing, and stopped the install
# trying to rewrite a key the vault already held.
#
# Two things are exercised, with bao_curl stubbed and no cluster:
#   - _resolve_bao_token keeps a token the vault accepts and drops one it
#     refuses, falling through to the next source;
#   - B-10's check calls the key missing only when the vault says so.
# =============================================================================

# shellcheck disable=SC2016 # the checks are strings the harness evaluates
# shellcheck disable=SC2034,SC2317,SC2329 # read and called by the code under test
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

CHECK_SATISFIED=0 CHECK_MISSING=1 CHECK_UNDEFINED=2
info() { :; }
warn() { printf '%s\n' "$*" >>"${WORK}/warnings"; }
success() { :; }

# The vault, as bao_curl answers it: STUB_CODE is the HTTP status, STUB_BODY
# the document. A call that discards the body (-o /dev/null) gets the status
# alone; one that asks for both gets the body, a newline and the status.
bao_curl() {
    local a
    for a in "$@"; do
        [[ "${a}" == "-o" ]] && { printf '%s' "${STUB_CODE}"; return 0; }
    done
    printf '%s\n%s' "${STUB_BODY:-}" "${STUB_CODE}"
}

# Only the functions under test, out of the library: sourcing all of it would
# pull in everything it sources in turn.
eval "$(sed -n '/^_bao_token_alive() {/,/^}/p; /^_resolve_bao_token() {/,/^}/p' scripts/lib/openbao.sh)"

echo ""
echo "_resolve_bao_token and an exported token"
echo ""

export BAO_ADDR="https://openbao.test:8200"
export GENTIAN_NONINTERACTIVE=1
OPENBAO_INIT_FILE="${WORK}/no-such-init.json"

resolve() {
    rc=0
    : >"${WORK}/warnings"
    export BAO_TOKEN="$1"
    STUB_CODE="$2" _resolve_bao_token || rc=$?
}

resolve live 200
check "a token the vault accepts is kept" '[[ ${rc} == 0 && "${BAO_TOKEN:-}" == live ]]'

resolve dead 403
check "an expired token is dropped" '[[ -z "${BAO_TOKEN:-}" ]]'
check "  and the next source is tried (none here, unattended)" '[[ ${rc} == 1 ]]'
check "  and the operator is told why" 'grep -q "no longer signs in" "${WORK}/warnings"'

resolve kept 000
check "no answer from the vault keeps the token" '[[ ${rc} == 0 && "${BAO_TOKEN:-}" == kept ]]'

BAO_ADDR="" resolve kept 403
check "no address to ask keeps the token" '[[ ${rc} == 0 && "${BAO_TOKEN:-}" == kept ]]'

echo ""
echo "B-10's check and the director's key"
echo ""

STEP="$(echo scripts/steps/[A-Z]-[0-9][0-9]-deployment-signing.sh)"
[[ -f "${STEP}" ]] || { echo "FAIL — no deployment-signing step found." >&2; exit 1; }
# The step defines check() too, which would replace the harness's: kept
# aside as step_check, and the harness's put back.
eval "$(declare -f check | sed '1s/^check/harness_check/')"
# shellcheck source=/dev/null
source "${STEP}"
eval "$(declare -f check | sed '1s/^check/step_check/')"
eval "$(declare -f harness_check | sed '1s/^harness_check/check/')"

# The keyring half passes, so every verdict below is the vault's.
GENTIAN_DEPLOYMENTS_PATH="${WORK}/deployments" GENTIAN_DEPLOYMENTS_CLUSTER_ID=test
mkdir -p "${WORK}/deployments/clusters/test/kernel/signing"
: >"${WORK}/deployments/clusters/test/kernel/signing/keys.env"
ns_kernel() { echo gitops; }
gentian_signing_keys_from_deployment() { echo AAAA; }
kubectl() { [[ "$*" == *jsonpath* ]] && echo "-----BEGIN PGP PUBLIC KEY BLOCK-----"; return 0; }

verdict() {
    rc=0
    : >"${WORK}/warnings"
    BAO_TOKEN=t STUB_CODE="$1" STUB_BODY="$2" step_check || rc=$?
}

verdict 200 '{"data":{"data":{"private":"k"}}}'
check "the key is in the vault: satisfied" '[[ ${rc} == ${CHECK_SATISFIED} ]]'

verdict 200 '{"data":{"data":{}}}'
check "the document has no key: missing" '[[ ${rc} == ${CHECK_MISSING} ]]'

verdict 404 ''
check "no such path: missing" '[[ ${rc} == ${CHECK_MISSING} ]]'

verdict 403 '{"errors":["permission denied"]}'
check "a refused token: undefined, not missing" '[[ ${rc} == ${CHECK_UNDEFINED} ]]'
check "  and the operator is told why" 'grep -q "HTTP 403" "${WORK}/warnings"'

verdict 000 ''
check "no answer: undefined, not missing" '[[ ${rc} == ${CHECK_UNDEFINED} ]]'

finish
