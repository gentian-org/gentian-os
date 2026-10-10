#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-install-settings.sh
# =============================================================================
# Where an installer setting comes from when more than one place could say.
# Each of these was a setting two places answered differently, and none of
# them failed an install: the wrong answer was used and nothing said so.
#
#   - an exported value beats install.env, for every variable and not for a
#     list of them; a default the installer assigns itself does not count as
#     exported, so install.env can still change it; a cluster setting in
#     install.env still beats the claim, and the installer still says so;
#   - certificates.issuerMode is read back from the claim file, so a re-run
#     of a self-signed cluster is not asked for a DNS credential;
#   - GENTIAN_OS_BRANCH unset is the branch of the checkout, wherever it is
#     read, and nothing when there is no branch; GENTIAN_UI_BRANCH and
#     PORTAL_IMAGE_TAG unset are both develop;
#   - GENTIAN_DEPLOYMENTS_REPO has no default: an unattended run without it
#     stops, and writes nothing first.
#
# Runs the library's own functions under set -u, the way the installer runs
# them, in a sandbox HOME against throwaway local git repositories. No
# cluster, no network.
# =============================================================================
# Every check is a string handed to the library's shell, so what it names is
# expanded there.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
mkdir -p "${SANDBOX}/home"
g() { git -c user.name=t -c user.email=t@t -c init.defaultBranch=main -c commit.gpgsign=false "$@" >/dev/null 2>&1; }

# run <env assignments...> -- <shell> : the library, then the shell given, in
# a fresh process with nothing inherited but HOME and PATH.
run() {
    local -a envs=()
    while [[ "$1" != "--" ]]; do envs+=("$1"); shift; done
    shift
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" \
        ${envs[@]+"${envs[@]}"} \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; '"$1" 2>&1
}

ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${2:-}"; fail=$((fail + 1)); }
is() { # <what> <output> <exactly>
    if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "    got:  $2
    want: $3"; fi
}
has() { # <what> <output> <must contain> [must not contain]
    if [[ "$2" == *"$3"* && "$2" != *"unbound variable"* && ( -z "${4:-}" || "$2" != *"$4"* ) ]]; then ok "$1"; else bad "$1" "$2"; fi
}

echo ""
echo "Installer settings: one answer, whichever place is asked"
echo ""

# --- the environment, install.env and the claim ------------------------------
ENVFILE="${SANDBOX}/install.env"
cat > "${ENVFILE}" <<'EOF'
GENTIAN_OS_BRANCH=from-file
GENTIAN_DEPLOYMENTS_STAGE=prod
GENTIAN_DEPLOYMENTS_CLUSTER_ID=c1
GENTIAN_NO_LICENCE_REPORT=1
GENTIAN_DISABLE_API_EXTENSIONS=1
NETWORK_MODE=static-ip
EOF
show='load_operator_config >/dev/null; echo "${GENTIAN_OS_BRANCH:-} ${GENTIAN_DEPLOYMENTS_STAGE:-} ${GENTIAN_DEPLOYMENTS_CLUSTER_ID:-} ${GENTIAN_NO_LICENCE_REPORT:-} ${GENTIAN_DISABLE_API_EXTENSIONS:-}"'

is "nothing exported: install.env answers, also where the installer has a default of its own" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" -- "${show}")" "from-file prod c1 1 1"
is "exported values beat install.env, whichever variable it is" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_OS_BRANCH=from-env GENTIAN_DEPLOYMENTS_STAGE=dev GENTIAN_DEPLOYMENTS_CLUSTER_ID=c2 -- "${show}")" \
    "from-env dev c2 1 1"
out="$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_OS_BRANCH=from-env -- 'load_operator_config')"
has "and the run says which value was used" "${out}" "GENTIAN_OS_BRANCH is set in the environment"
has "naming only what differs" "${out}" "Loaded installer config" "PATH is set"
out="$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_OS_BRANCH=from-file -- 'load_operator_config')"
has "an exported value equal to the file's is not reported" "${out}" "Loaded installer config" "is set in the environment"
is "an exported empty value is not a value: install.env answers" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_OS_BRANCH= -- "${show}")" "from-file prod c1 1 1"
is "--no-config-files: install.env is not read" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" INSTALL_AUTO_LOAD_CONFIG=0 -- "${show}")" "    0"

CO="${SANDBOX}/co"
mkdir -p "${CO}/clusters/c1/kernel/claims"
claim() { # <spec lines...>
    { printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c1-dev\nspec:\n  kernelDomain: k.example\n'
      printf '%s\n' "$@"; } > "${CO}/clusters/c1/kernel/claims/cluster.yaml"
}
claim "  networkMode: tunnel"
settings='load_operator_config >/dev/null; load_deployments_cluster_settings'
out="$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_DEPLOYMENTS_PATH="${CO}" -- "${settings}"'; echo "mode=${NETWORK_MODE:-}"')"
has "a cluster setting in install.env beats the claim" "${out}" "mode=static-ip"
has "and the installer says so" "${out}" "NETWORK_MODE is set in ${ENVFILE} — it overrides claims/cluster.yaml"
out="$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_DEPLOYMENTS_PATH="${CO}" NETWORK_MODE=tunnel -- "${settings}"'; echo "mode=${NETWORK_MODE:-}"')"
has "an exported cluster setting beats both" "${out}" "mode=tunnel"

# --- the issuer mode, from the claim file ------------------------------------
printf 'GENTIAN_DEPLOYMENTS_CLUSTER_ID=c1\n' > "${ENVFILE}"
issuer="${settings}"' >/dev/null 2>&1; echo "${CERT_ISSUER_MODE:-unset}"'
claim "  certificates:" "    issuerMode: self-signed"
is "issuerMode: self-signed on the claim is what a re-run uses" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_DEPLOYMENTS_PATH="${CO}" -- "${issuer}")" "self-signed"
is "so the DNS credential is not one this cluster is asked for" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_DEPLOYMENTS_PATH="${CO}" DNS_PROVIDER=cloudflare -- \
        "${settings}"' >/dev/null 2>&1; _requirement_applies acme-dns-cloudflare && echo asked || echo not-asked')" "not-asked"
claim "  certificates:" "    issuerMode: acme-dns01"
is "while under acme-dns01 it is" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_DEPLOYMENTS_PATH="${CO}" DNS_PROVIDER=cloudflare -- \
        "${settings}"' >/dev/null 2>&1; _requirement_applies acme-dns-cloudflare && echo asked || echo not-asked')" "asked"
claim "  certificates:" "    issuerMode: acme-http01"
is "an exported CERT_ISSUER_MODE still beats the claim" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_DEPLOYMENTS_PATH="${CO}" CERT_ISSUER_MODE=private-ca -- "${issuer}")" "private-ca"
claim "  networkMode: tunnel"
is "a claim that does not say takes the schema's default" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_DEPLOYMENTS_PATH="${CO}" -- "${issuer}")" "acme-dns01"
rm -f "${CO}/clusters/c1/kernel/claims/cluster.yaml"
is "no claim yet: nothing is assumed, and step 0 asks" \
    "$(run INSTALL_CONFIG_FILE="${ENVFILE}" GENTIAN_DEPLOYMENTS_PATH="${CO}" -- "${issuer}")" "unset"

# --- the refs ----------------------------------------------------------------
# A stand-in for the gentian-os checkout the installer is run from, and for
# the remote it is checked against: the ref has to exist there.
OS="${SANDBOX}/os"; OSREMOTE="${SANDBOX}/os.git"
g init --bare -b feature-x "${OSREMOTE}"
g clone "${OSREMOTE}" "${OS}"; g -C "${OS}" checkout -b feature-x
echo os > "${OS}/f"; g -C "${OS}" add -A; g -C "${OS}" commit -m os
g -C "${OS}" push origin feature-x; g -C "${OS}" tag v9.9.9; g -C "${OS}" push origin v9.9.9
at() { printf 'SCRIPT_DIR=%q; ' "${OS}"; }   # the libraries are loaded; now the checkout is the stand-in

is "GENTIAN_OS_BRANCH unset: the branch of the checkout" "$(run -- "$(at)gentian_os_ref")" "feature-x"
is "GENTIAN_OS_BRANCH set: that" "$(run GENTIAN_OS_BRANCH=v9.9.9 -- "$(at)gentian_os_ref")" "v9.9.9"
is "the installer resolves it to the same" \
    "$(run GENTIAN_OS_REPO="${OSREMOTE}" -- "$(at)"'resolve_gentian_os_branch >/dev/null 2>&1; echo "${GENTIAN_OS_BRANCH:-unset}"')" "feature-x"
is "also when the image tag is given, which used to skip it" \
    "$(run GENTIAN_OS_REPO="${OSREMOTE}" GENTIAN_OS_IMAGE_TAG=1.2.3 -- "$(at)"'resolve_gentian_os_image_tag >/dev/null 2>&1; echo "${GENTIAN_OS_BRANCH:-unset} ${GENTIAN_OS_IMAGE_TAG}"')" \
    "feature-x 1.2.3"
g -C "${OS}" checkout --detach
is "a detached checkout has no branch to read" "$(run -- "$(at)gentian_os_ref")" ""
out="$(run GENTIAN_OS_REPO="${OSREMOTE}" -- "$(at)"'resolve_gentian_os_branch; echo "rc=$?"')"
has "and the installer stops and asks for the ref, not guessing one" "${out}" "GENTIAN_OS_BRANCH is not set and this checkout has no branch to read" "rc=0"
# Step 0 refuses before it writes anything: the Repository claim would name it.
DEP="${SANDBOX}/dep"; g init -b main "${DEP}"
out="$(run GENTIAN_DEPLOYMENTS_PATH="${DEP}" GENTIAN_DEPLOYMENTS_CLUSTER_ID=c1 KERNEL_DOMAIN=k.example -- "$(at)"'scaffold_cluster_deployment; echo "rc=$?"')"
has "step 0 does not write a Repository claim naming a ref nobody chose" "${out}" "the gentian-os Repository claim cannot name the ref" "rc=0"
if [[ ! -e "${DEP}/clusters" ]]; then ok "and has written nothing"; else bad "and has written nothing" "$(find "${DEP}/clusters")"; fi

is "GENTIAN_UI_BRANCH unset: develop" "$(run -- gentian_ui_branch)" "develop"
is "PORTAL_IMAGE_TAG unset: develop, the same" "$(run -- gentian_ui_chart_branch)" "develop"
is "each set: that" "$(run GENTIAN_UI_BRANCH=a PORTAL_IMAGE_TAG=b -- 'echo "$(gentian_ui_branch) $(gentian_ui_chart_branch)"')" "a b"
# One default each means no second one beside it.
second="$(grep -nE '\$\{(GENTIAN_OS_BRANCH|GENTIAN_UI_BRANCH|PORTAL_IMAGE_TAG):-[^}]' install.sh scripts/lib/*.sh scripts/steps/*.sh \
    | grep -vE 'common\.sh:[0-9]+:(gentian_ui_branch|gentian_ui_chart_branch)\(\)|local ref="\$\{GENTIAN_OS_BRANCH:-\}"' || true)"
if [[ -z "${second}" ]]; then ok "no step or library carries a default of its own for any of the three"; else bad "no step or library carries a default of its own for any of the three" "${second}"; fi

# --- the deployments repository ----------------------------------------------
rm -rf "${SANDBOX:?}/home"; mkdir -p "${SANDBOX}/home"
out="$(run GENTIAN_NONINTERACTIVE=1 -- 'prompt_app_repos; echo "rc=$?"')"
has "unattended and no GENTIAN_DEPLOYMENTS_REPO: the run stops and says what to set" "${out}" "GENTIAN_DEPLOYMENTS_REPO is not set and GENTIAN_NONINTERACTIVE=1" "rc="
if [[ ! -e "${SANDBOX}/home/.gentian" ]]; then ok "and nothing was written under ~/.gentian"; else bad "and nothing was written under ~/.gentian"; fi
out="$(run GENTIAN_NONINTERACTIVE=1 GENTIAN_DEPLOYMENTS_REPO=https://git.example.test/d -- 'prompt_app_repos >/dev/null; echo "${GENTIAN_DEPLOYMENTS_REPO} ${GENTIAN_DEPLOYMENTS_BRANCH}"')"
is "with it, the run goes on with that address" "${out}" "https://git.example.test/d main"
if ! grep -q 'git\.example\.domain' scripts/lib/common.sh; then ok "the example address is no longer in the installer"; else bad "the example address is no longer in the installer"; fi

echo ""
if [[ ${fail} -eq 0 ]]; then
    echo "${GREEN}${pass} checks passed.${NC}"
    exit 0
fi
echo "${RED}${fail} failed${NC}, ${pass} passed."
exit 1
