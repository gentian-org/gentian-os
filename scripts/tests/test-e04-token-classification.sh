#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-e04-token-classification.sh
# =============================================================================
# E-04 revokes the bootstrap credential. After handover the installer no longer
# has one — _resolve_bao_token signs in via OIDC instead — so "a token
# authenticates" stopped meaning "the bootstrap token is live", and the step
# reported every later run as an unfinished cluster.
#
# The distinction is the token's policies, which is what these exercise. `bao`
# is a stub on PATH: no cluster, no OpenBao, and the classification is pure
# function of what the CLI answers.
# =============================================================================

set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT
cat > "${STUB_DIR}/bao" <<'STUB'
#!/usr/bin/env bash
# token lookup -format=json → whatever BAO_STUB_POLICIES asks for.
case "${BAO_STUB_POLICIES:-}" in
    unauthenticated) exit 1 ;;
    silent)          echo '{"data":{}}' ;;
    *)               printf '{"data":{"policies":[%s]}}\n' "${BAO_STUB_POLICIES}" ;;
esac
STUB
chmod +x "${STUB_DIR}/bao"
PATH="${STUB_DIR}:${PATH}"

# Only the classifier is under test, so it is sourced out of the step rather
# than the step being run — which would need a cluster for every other guard.
eval "$(sed -n '/^_token_is_root() {/,/^}/p' scripts/steps/E-04-revoke-bootstrap-token.sh)"

expect() {
    local label="$1" policies="$2" want="$3" got=0
    BAO_STUB_POLICIES="${policies}" _token_is_root || got=$?
    if [[ "${got}" == "${want}" ]]; then
        printf '  %s✓%s %-46s rc=%s\n' "${GREEN}" "${NC}" "${label}" "${got}"
        pass=$((pass + 1))
    else
        printf '  %s✗%s %-46s want rc=%s got rc=%s\n' "${RED}" "${NC}" "${label}" "${want}" "${got}"
        fail=$((fail + 1))
    fi
}

echo ""
echo "E-04 token classification"
echo ""

# The bootstrap credential: the one thing this step exists to revoke.
expect "root token"                      '"root"'                    0
# What every run after handover carries. Reported as work to do for weeks.
expect "OIDC cluster-admin session"      '"cluster-admin","default"' 1
expect "some other non-root token"       '"default"'                 1
# Already revoked, or a shell with a dead token: finished, not unknown.
expect "token that does not authenticate" 'unauthenticated'          1
# Neither answer. Guessing either way is what this step refuses to do.
expect "policies not reported"           'silent'                    2

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sAll %d classifications correct.%s\n' "${GREEN}" "${pass}" "${NC}"
