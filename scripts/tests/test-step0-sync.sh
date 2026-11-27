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

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

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
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" GENTIAN_DEPLOYMENTS_BRANCH="${TEST_BRANCH:-main}" \
        ${GENTIAN_DEPLOYMENTS_REPO:+GENTIAN_DEPLOYMENTS_REPO="${GENTIAN_DEPLOYMENTS_REPO}"} \
        ${GENTIAN_DEPLOYMENTS_PATH:+GENTIAN_DEPLOYMENTS_PATH="${GENTIAN_DEPLOYMENTS_PATH}"} \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; rc=0; gentian_sync_deployments_checkout '"${1:-}"' || rc=$?; echo "rc=${rc}"' 2>&1
}

expect() {
    local what="$1" out="$2" want_rc="$3" want_text="$4"
    if [[ "${out}" == *"rc=${want_rc}"* && "${out}" == *"${want_text}"* && "${out}" != *"unbound variable"* ]]; then
        ok "${what}"
    else
        bad "${what}" "${out}"
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
export GENTIAN_DEPLOYMENTS_PATH="${CHECKOUT}"
expect "no checkout and no repository named: leaves it to the scaffold to say so" "$(sync)" 0 ""
[[ ! -e "${CHECKOUT}" ]] || { echo "  something was made with no repository named"; fail=$((fail + 1)); }

# With the repository named, a missing or empty checkout is cloned. The path
# is named too, as an install.env for a second cluster names it: unnamed, a
# sibling ../gentian-deployments of the checkout would be taken instead.
export GENTIAN_DEPLOYMENTS_REPO="${SANDBOX}/origin.git" GENTIAN_DEPLOYMENTS_PATH="${CHECKOUT}"
expect "--validate / --dry-run with no checkout: says it would clone" "$(sync check)" 0 "Would clone"
[[ ! -e "${CHECKOUT}" ]] || { echo "  a check made the checkout"; fail=$((fail + 1)); }

expect "no checkout: clones it" "$(sync)" 0 "cloning"
[[ "$(git -C "${CHECKOUT}" log -1 --format=%s 2>/dev/null)" == "four" ]] || { echo "  the clone is not at origin/main"; fail=$((fail + 1)); }
[[ "$(git -C "${CHECKOUT}" branch --show-current 2>/dev/null)" == "main" ]] || { echo "  the clone is not on main"; fail=$((fail + 1)); }

rm -rf "${CHECKOUT}"; mkdir -p "${CHECKOUT}"
expect "an empty directory: clones into it" "$(sync)" 0 "cloning"
[[ -d "${CHECKOUT}/.git" ]] || { echo "  the empty directory was not cloned into"; fail=$((fail + 1)); }

rm -rf "${CHECKOUT}"; mkdir -p "${CHECKOUT}"; echo mine > "${CHECKOUT}/notes"
expect "a directory that holds files: left alone" "$(sync)" 0 ""
[[ ! -e "${CHECKOUT}/.git" && "$(cat "${CHECKOUT}/notes")" == "mine" ]] || { echo "  a directory with files was touched"; fail=$((fail + 1)); }

rm -rf "${CHECKOUT}"
GENTIAN_DEPLOYMENTS_REPO="${SANDBOX}/nowhere.git"
expect "a clone that fails: stops and says how to clone by hand" "$(sync)" 1 "git clone --branch main ${SANDBOX}/nowhere.git"

# A branch origin does not have yet: cloned from the default branch, and the
# branch started in the checkout for step 0's push to create.
rm -rf "${CHECKOUT}"
GENTIAN_DEPLOYMENTS_REPO="${SANDBOX}/origin.git"
export TEST_BRANCH=cloud
expect "--validate / --dry-run, a branch origin lacks: says it would start it" "$(sync check)" 0 "start branch cloud"
[[ ! -e "${CHECKOUT}" ]] || { echo "  a check made the checkout"; fail=$((fail + 1)); }
expect "a branch origin lacks: clones and starts it" "$(sync)" 0 "step 0's push creates it"
[[ "$(git -C "${CHECKOUT}" branch --show-current 2>/dev/null)" == "cloud" ]] || { echo "  the checkout is not on cloud"; fail=$((fail + 1)); }
[[ "$(git -C "${CHECKOUT}" log -1 --format=%s 2>/dev/null)" == "four" ]] || { echo "  cloud did not start at origin/main"; fail=$((fail + 1)); }
git --git-dir="${SANDBOX}/origin.git" rev-parse -q --verify refs/heads/cloud >/dev/null &&
    { echo "  the branch reached origin before step 0 pushed it"; fail=$((fail + 1)); }

# An empty repository, which GETTING-STARTED says is enough: no branch at all.
rm -rf "${CHECKOUT}"
g init --bare -b main "${SANDBOX}/empty.git"
GENTIAN_DEPLOYMENTS_REPO="${SANDBOX}/empty.git" TEST_BRANCH=main
expect "an empty repository: clones it and starts the branch" "$(sync)" 0 "step 0's push creates it"
[[ "$(git -C "${CHECKOUT}" branch --show-current 2>/dev/null)" == "main" ]] || { echo "  the empty clone is not on main"; fail=$((fail + 1)); }

# Origin cannot be asked: no branch is started on a guess, git says why.
rm -rf "${CHECKOUT}"
GENTIAN_DEPLOYMENTS_REPO="${SANDBOX}/nowhere.git" TEST_BRANCH=cloud
expect "origin cannot be asked: no branch started, the clone fails" "$(sync)" 1 "git clone --branch cloud ${SANDBOX}/nowhere.git"
unset GENTIAN_DEPLOYMENTS_REPO GENTIAN_DEPLOYMENTS_PATH TEST_BRANCH

finish
