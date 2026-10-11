#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-catalogue-default.sh
# =============================================================================
# A new Cluster claim names one catalogue, `gentian`, and gentian-apps
# publishes two addresses for it: the released catalogue and the development
# one. Which the claim is given follows from which gentian-os is being
# installed, and getting it wrong is quiet -- a released platform offered
# profiles it cannot read, or a development cluster that never sees the
# profiles written for it:
#
#   - a release tag, or main: the released catalogue;
#   - any other branch: the development catalogue;
#   - GENTIAN_CATALOGUE_URL, set: that address, whatever the ref;
#   - the claim says which and why, in a comment above the address;
#   - a claim that names a catalogue already is left exactly as it is.
#
# Runs the library's own functions under set -u, the way the installer runs
# them. No cluster, no network.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
mkdir -p "${SANDBOX}/home"

RELEASED="https://gentian-org.github.io/gentian-apps"
DEVELOP="https://gentian-org.github.io/gentian-apps/develop"

# run <env assignments...> -- <shell> : the library, then the shell given, in
# a fresh process with nothing inherited but HOME and PATH.
run() {
    local -a envs=()
    while [[ "$1" != "--" ]]; do envs+=("$1"); shift; done
    shift
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" \
        KERNEL_DOMAIN=k.example ${envs[@]+"${envs[@]}"} \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e; '"$1" 2>&1
}

ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${2:-}"; fail=$((fail + 1)); }
is() { # <what> <output> <exactly>
    if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1" "    got:  $2
    want: $3"; fi
}

echo ""
echo "Step 0: the default catalogue, by what is being installed"
echo ""

is "a release tag reads the released catalogue" \
    "$(run GENTIAN_OS_BRANCH=v0.5.0 -- gentian_catalogue_url)" "${RELEASED}"
is "a release candidate tag reads the released catalogue" \
    "$(run GENTIAN_OS_BRANCH=v0.5.0-rc.1 -- gentian_catalogue_url)" "${RELEASED}"
is "main reads the released catalogue" \
    "$(run GENTIAN_OS_BRANCH=main -- gentian_catalogue_url)" "${RELEASED}"
is "develop reads the development catalogue" \
    "$(run GENTIAN_OS_BRANCH=develop -- gentian_catalogue_url)" "${DEVELOP}"
is "test-cb reads the development catalogue" \
    "$(run GENTIAN_OS_BRANCH=test-cb -- gentian_catalogue_url)" "${DEVELOP}"
is "a feature branch reads the development catalogue" \
    "$(run GENTIAN_OS_BRANCH=feat/x -- gentian_catalogue_url)" "${DEVELOP}"
is "a branch that only begins like a tag is a branch" \
    "$(run GENTIAN_OS_BRANCH=v05 -- gentian_catalogue_url)" "${DEVELOP}"
is "GENTIAN_CATALOGUE_URL wins over a branch" \
    "$(run GENTIAN_OS_BRANCH=test-cb GENTIAN_CATALOGUE_URL=https://c.example/x -- gentian_catalogue_url)" "https://c.example/x"
is "GENTIAN_CATALOGUE_URL wins over a release tag" \
    "$(run GENTIAN_OS_BRANCH=v0.5.0 GENTIAN_CATALOGUE_URL=https://c.example/x -- gentian_catalogue_url)" "https://c.example/x"

# No setting: the checkout's branch decides, as it does for the ref the
# cluster follows. A checkout with no branch to read gets the released one.
CO="${SANDBOX}/co"
g() { git -c user.name=t -c user.email=t@t -c commit.gpgsign=false "$@" >/dev/null 2>&1; }
g init -b test-cb "${CO}"; g -C "${CO}" commit --allow-empty -m seed
is "no setting, a checkout on a branch: the development catalogue" \
    "$(run -- "SCRIPT_DIR='${CO}' gentian_catalogue_url")" "${DEVELOP}"
g -C "${CO}" checkout --detach
is "no setting, a detached checkout: the released catalogue" \
    "$(run -- "SCRIPT_DIR='${CO}' gentian_catalogue_url")" "${RELEASED}"

# What reaches the claim: the address, and above it the reason.
section="$(run GENTIAN_OS_BRANCH=test-cb -- _claim_catalogue_section)"
is "the claim names the development catalogue for a branch" \
    "$(grep -E '^        url: ' <<<"${section}")" "        url: ${DEVELOP}"
is "the claim says why, above the address" \
    "$(grep -B1 -E '^        url: ' <<<"${section}" | head -n 1)" \
    "        # The development catalogue: this cluster was installed from the gentian-os branch test-cb, not from a release."
section="$(run GENTIAN_OS_BRANCH=v0.5.0 -- _claim_catalogue_section)"
is "the claim names the released catalogue for a release" \
    "$(grep -E '^        url: ' <<<"${section}")" "        url: ${RELEASED}"
is "and says which release" \
    "$(grep -B1 -E '^        url: ' <<<"${section}" | head -n 1)" \
    "        # The released catalogue: this cluster was installed from gentian-os v0.5.0."
section="$(run GENTIAN_OS_BRANCH=v0.5.0 GENTIAN_CATALOGUE_URL=https://c.example/x -- _claim_catalogue_section)"
is "an address somebody set is recorded as set" \
    "$(grep -B1 -E '^        url: ' <<<"${section}" | tr '\n' '|')" \
    "        # Set by GENTIAN_CATALOGUE_URL when this claim was written.|        url: https://c.example/x|"

# The section parses, and sits where the claim expects it.
if command -v yq >/dev/null 2>&1; then
    printf 'spec:%s\n' "$(run GENTIAN_OS_BRANCH=test-cb -- _claim_catalogue_section)" > "${SANDBOX}/new.yaml"
    is "the section is yaml, with the address under spec.catalogue.sources" \
        "$(run -- "yq_get '.spec.catalogue.sources[0].url' '${SANDBOX}/new.yaml'")" "${DEVELOP}"
fi

# An existing claim is somebody's: installing from another ref changes nothing.
CLAIM="${SANDBOX}/cluster.yaml"
printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nspec:\n  catalogue:\n    sources:\n      - name: gentian\n        url: %s\n' "${RELEASED}" > "${CLAIM}"
before="$(cat "${CLAIM}")"
run GENTIAN_OS_BRANCH=test-cb -- "ensure_claim_catalogue_section '${CLAIM}'" >/dev/null
is "a claim that names a catalogue is not rewritten" "$(cat "${CLAIM}")" "${before}"

# Two addresses earlier installs wrote never served anything. A claim that
# still names them is left as it is, and the installer says so.
if command -v yq >/dev/null 2>&1; then
    said="$(run GENTIAN_OS_BRANCH=test-cb -- "ensure_claim_catalogue_section '${CLAIM}'")"
    is "a claim naming a catalogue that serves is not warned about" \
        "$(grep -c 'never served' <<<"${said}")" "0"
    printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nspec:\n  catalogue:\n    storeUrl: https://gentian.org/apps\n    sources:\n      - name: gentian\n        url: https://store.gentian.org/catalogue\n' > "${CLAIM}"
    before="$(cat "${CLAIM}")"
    said="$(run GENTIAN_OS_BRANCH=test-cb -- "ensure_claim_catalogue_section '${CLAIM}'")"
    is "a claim naming the retired store and catalogue is warned about, once each" \
        "$(grep -c 'never served' <<<"${said}")" "2"
    is "the warning names the store a new claim would name" \
        "$(grep -c 'https://store-service.aluvian.io' <<<"${said}")" "1"
    is "and the catalogue a new claim would name" \
        "$(grep -c "${DEVELOP}" <<<"${said}")" "1"
    is "the claim itself is left as it is" "$(cat "${CLAIM}")" "${before}"
fi

# certificates.externalDns is no field of the claim any more, and step 0 wrote
# it into every static-ip claim: it is removed from a claim written before,
# and nothing else of the claim moves.
printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nspec:\n  certificates:\n    issuerMode: acme-dns01\n    dnsProvider: cloudflare\n    # Set false where something else already owns these records.\n    externalDns: true\n  catalogue:\n    sources:\n      - name: gentian\n        url: %s\n' "${RELEASED}" > "${CLAIM}"
want="$(grep -v '^    externalDns: true$' "${CLAIM}")"
said="$(run GENTIAN_OS_BRANCH=test-cb -- "ensure_claim_catalogue_section '${CLAIM}'")"
is "a claim that carries certificates.externalDns loses that line and no other" "$(cat "${CLAIM}")" "${want}"
is "and the installer says so" "$(grep -c 'removed certificates.externalDns' <<<"${said}")" "1"
said="$(run GENTIAN_OS_BRANCH=test-cb -- "ensure_claim_catalogue_section '${CLAIM}'")"
is "a second run finds nothing to remove" "$(grep -c 'externalDns' <<<"${said}")" "0"
printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nspec:\n  certificates:\n    issuerMode: acme-dns01\n    # externalDns: true         let external-dns write this zone.\n  catalogue: {}\n' > "${CLAIM}"
before="$(cat "${CLAIM}")"
run GENTIAN_OS_BRANCH=test-cb -- "ensure_claim_catalogue_section '${CLAIM}'" >/dev/null
is "a claim that only mentions it in a comment is left as it is" "$(cat "${CLAIM}")" "${before}"

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sAll %d cases correct.%s\n' "${GREEN}" "${pass}" "${NC}"
