#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-step0-sync.sh
# =============================================================================
# Step 0 brings the deployments checkout up to date with its remote before the
# claim in it is read. It runs before anything else has settled where that
# checkout is, so it has to settle it itself: it read the variable raw, and on
# a host whose install.env names no path the installer stopped on its first
# line with "GENTIAN_DEPLOYMENTS_PATH: unbound variable".
#
# Every case runs with the variable UNSET and under set -u, which is how the
# installer runs and what the first version of this was never tested under.
# The checkout lives where the default puts it, in a throwaway HOME.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main "$@" >/dev/null 2>&1; }

# A remote, a second clone that moves it, and the checkout step 0 reads, at
# the default path under the sandbox HOME.
g init --bare -b main "${SANDBOX}/origin.git"
g clone "${SANDBOX}/origin.git" "${SANDBOX}/other"
g -C "${SANDBOX}/other" checkout -b main
echo 1 > "${SANDBOX}/other/f"; g -C "${SANDBOX}/other" add -A; g -C "${SANDBOX}/other" commit -m one
g -C "${SANDBOX}/other" push origin main
CHECKOUT="${SANDBOX}/home/.gentian/gentian-deployments"
mkdir -p "${SANDBOX}/home/.gentian"
g clone -b main "${SANDBOX}/origin.git" "${CHECKOUT}"
advance() {
    echo "$1" > "${SANDBOX}/other/f"; g -C "${SANDBOX}/other" commit -am "$1"
    g -C "${SANDBOX}/other" push origin main
}

# One sync, in a fresh shell, the way the installer calls it: nothing but HOME
# and PATH, so no deployments path is set by anybody. set -u is switched on
# inside rather than with bash -u, as install.sh does it: the host's own
# startup files are not ours to hold to it.
sync() {
    # shellcheck disable=SC2016 # the inner script is for the child shell to expand
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" GENTIAN_DEPLOYMENTS_BRANCH=main \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; rc=0; gentian_sync_deployments_checkout '"${1:-}"' || rc=$?; echo "rc=${rc}"' 2>&1
}

expect() {
    local what="$1" out="$2" want_rc="$3" want_text="$4"
    if [[ "${out}" == *"rc=${want_rc}"* && "${out}" == *"${want_text}"* && "${out}" != *"unbound variable"* ]]; then
        printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "${what}"; pass=$((pass + 1))
    else
        printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "${what}" "${out}"; fail=$((fail + 1))
    fi
}

echo ""
echo "Step 0: the deployments checkout is synced before it is read"
echo ""

advance two
expect "behind and clean, with no path configured: fast-forwards" "$(sync)" 0 "fast-forwarded"
[[ "$(git -C "${CHECKOUT}" log -1 --format=%s)" == "two" ]] || { echo "  the checkout did not move"; fail=$((fail + 1)); }

advance three
expect "--validate / --dry-run only report it" "$(sync check)" 0 "behind origin/main"

expect "the next install catches up" "$(sync)" 0 "fast-forwarded"
out="$(sync)"
if [[ "${out}" == "rc=0" ]]; then
    printf '  %sok%s    up to date: says nothing\n' "${GREEN}" "${NC}"; pass=$((pass + 1))
else
    printf '  %sFAIL%s  up to date: says nothing\n%s\n' "${RED}" "${NC}" "${out}"; fail=$((fail + 1))
fi

# Diverged: a local commit origin does not have, and origin has moved on.
echo local > "${CHECKOUT}/g"; g -C "${CHECKOUT}" add -A; g -C "${CHECKOUT}" commit -m local
advance four
expect "behind with a local commit: refuses, names both ways out" "$(sync)" 1 "reset --hard origin/main"
[[ "$(git -C "${CHECKOUT}" log -1 --format=%s)" == "local" ]] || { echo "  a refused sync moved the checkout"; fail=$((fail + 1)); }

rm -rf "${CHECKOUT}"
expect "no checkout at all: leaves it to the scaffold to say so" "$(sync)" 0 ""

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sAll %d cases correct.%s\n' "${GREEN}" "${pass}" "${NC}"
