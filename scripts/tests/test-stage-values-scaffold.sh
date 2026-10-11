#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-stage-values-scaffold.sh
# =============================================================================
# Argo CD layers profiles/_base.yaml and profiles/<stage>.yaml under the
# platform's own chart and renders nothing while one is missing. Step 0 writes
# a missing one, so that an empty deployments repository is enough to install
# from. What is worth holding still:
#
#   - both are written when absent, each a values file that sets nothing;
#   - one that exists is never rewritten;
#   - the stage is the run's, and no other stage's file is written;
#   - both are among what step 0 commits, and only when they exist -- a path
#     that does not exist fails `git add` for every path with it.
#
# Runs the library's own functions in a throwaway deployments checkout, under
# set -u, the way the installer runs them. No cluster.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null 2>&1; }

CLUSTER=c1
new_checkout() {
    local dir="$1"
    rm -rf "${dir}"
    mkdir -p "${dir}"
    g init -b main "${dir}"
    g -C "${dir}" commit --allow-empty -m seed
}

# run <checkout> <env assignments...> -- <shell> : the library, then the shell
# given, in a fresh process with nothing inherited but HOME and PATH.
run() {
    local checkout="$1"; shift
    local -a envs=()
    while [[ "$1" != "--" ]]; do envs+=("$1"); shift; done
    shift
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" \
        GENTIAN_DEPLOYMENTS_PATH="${checkout}" GENTIAN_DEPLOYMENTS_CLUSTER_ID="${CLUSTER}" \
        KERNEL_DOMAIN=k.example ${envs[@]+"${envs[@]}"} \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; '"$1" 2>&1
}

ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${2:-}"; fail=$((fail + 1)); }
check() { # <what> <condition, as a command>
    local what="$1"; shift
    if "$@"; then ok "${what}"; else bad "${what}"; fi
}
# empty_values <file> — a YAML document that is an empty map, comments aside.
empty_values() {
    [[ -f "$1" ]] || return 1
    [[ "$(grep -v -E '^[[:space:]]*(#|$)' "$1")" == "{}" ]]
}

echo ""
echo "Step 0: the stage's values files"
echo ""

CO="${SANDBOX}/co"

new_checkout "${CO}"
out="$(run "${CO}" GENTIAN_DEPLOYMENTS_STAGE=prod -- '_scaffold_stage_values prod')"
check "an empty repository gets profiles/_base.yaml, setting nothing" empty_values "${CO}/profiles/_base.yaml"
check "and profiles/<stage>.yaml for the run's stage, setting nothing" empty_values "${CO}/profiles/prod.yaml"
if [[ ! -e "${CO}/profiles/dev.yaml" && ! -e "${CO}/profiles/staging.yaml" ]]; then
    ok "no other stage's file is written"
else
    bad "no other stage's file is written" "$(ls "${CO}/profiles")"
fi
if [[ "${out}" != *"unbound variable"* ]]; then ok "under set -u"; else bad "under set -u" "${out}"; fi
if grep -q 'stage "prod"' "${CO}/profiles/prod.yaml"; then
    ok "the stage file says which stage it is for"
else
    bad "the stage file says which stage it is for" "$(cat "${CO}/profiles/prod.yaml")"
fi

printf 'logLevel: debug\n' > "${CO}/profiles/prod.yaml"
printf '# mine\nreplicas: 2\n' > "${CO}/profiles/_base.yaml"
run "${CO}" GENTIAN_DEPLOYMENTS_STAGE=prod -- '_scaffold_stage_values prod' >/dev/null
if [[ "$(cat "${CO}/profiles/prod.yaml")" == "logLevel: debug" && "$(cat "${CO}/profiles/_base.yaml")" == $'# mine\nreplicas: 2' ]]; then
    ok "a file that exists is left exactly as it is"
else
    bad "a file that exists is left exactly as it is" "$(cat "${CO}/profiles/prod.yaml" "${CO}/profiles/_base.yaml")"
fi

paths="$(run "${CO}" GENTIAN_DEPLOYMENTS_STAGE=prod TENANCY_MODE=multi -- "_cluster_scaffold_paths ${CLUSTER}")"
if [[ "${paths}" == *"profiles/_base.yaml"* && "${paths}" == *"profiles/prod.yaml"* && "${paths}" != *"profiles/dev.yaml"* ]]; then
    ok "both are among what step 0 commits, for the run's stage only"
else
    bad "both are among what step 0 commits, for the run's stage only" "${paths}"
fi

new_checkout "${CO}"
paths="$(run "${CO}" GENTIAN_DEPLOYMENTS_STAGE=prod TENANCY_MODE=multi -- "_cluster_scaffold_paths ${CLUSTER}")"
if [[ "${paths}" != *"profiles/"* && "${paths}" == *"clusters/${CLUSTER}/kernel"* ]]; then
    ok "and are not named while they do not exist"
else
    bad "and are not named while they do not exist" "${paths}"
fi

# The files as Helm reads them: a values file must parse to a map.
new_checkout "${CO}"
run "${CO}" -- '_scaffold_stage_values dev' >/dev/null
if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' 2>/dev/null; then
    if python3 - "${CO}/profiles/_base.yaml" "${CO}/profiles/dev.yaml" <<'PY'
import sys, yaml
for f in sys.argv[1:]:
    assert yaml.safe_load(open(f)) == {}, f
PY
    then ok "both parse as an empty map"; else bad "both parse as an empty map"; fi
fi

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sAll %d cases correct.%s\n' "${GREEN}" "${pass}" "${NC}"
