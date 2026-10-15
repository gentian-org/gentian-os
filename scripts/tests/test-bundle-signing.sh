#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-bundle-signing.sh
# =============================================================================
# A cluster signs its bundles with a key of its own and records an id of its
# own in them; a restore brings back the rights granted in a tenant only from
# a bundle that both say is this cluster's. B-08 seeds the key into the vault
# once, at random or from the recovery kit, never replaces it, and never
# passes it as an argument; the kit carries it. Step 0 gives the Cluster
# claim its id once and never changes it.
#
# bao is a shell function over files in a throwaway directory. No cluster
# and no vault is touched.
# =============================================================================
# Every check is a condition string evaluated after the run it inspects:
# shellcheck disable=SC2016,SC2034 # and the variables they read are set for them
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
REPO="$(pwd)"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT
for tool in openssl jq; do
    command -v "${tool}" >/dev/null || { echo "${tool} required"; exit 1; }
done

# The vault is vault/<path>.json; bao-args.log records every bao argument list.
new_state() {
    STATE="$(mktemp -d "${SANDBOX}/state.XXXXXX")"
    mkdir -p "${STATE}/vault"
    : > "${STATE}/bao-args.log"
}

# run <script> [VAR=value ...]
run() {
    local script="$1"; shift
    env -i HOME="${SANDBOX}/home" PATH="${PATH}" SCRIPT_DIR="${REPO}" STATE="${STATE}" \
        KERNEL_DOMAIN=example.test SCRIPT="${script}" "$@" \
        bash -c '
            source "${SCRIPT_DIR}/scripts/lib/load.sh" >/dev/null 2>&1
            trap - ERR; set +e
            kubectl() { return 1; }
            bao() {
                printf "%s\n" "$*" >> "${STATE}/bao-args.log"
                local path="${*: -1}" field="" a
                for a in "$@"; do [[ "${a}" == -field=* ]] && field="${a#-field=}"; done
                case "$1 $2" in
                    "kv get")
                        [[ -f "${STATE}/vault/${path//\//_}.json" ]] || return 2
                        jq -r --arg f "${field}" ".[\$f] // empty" "${STATE}/vault/${path//\//_}.json" ;;
                    "kv put")
                        path="${*: -2:1}"
                        [[ -f "${STATE}/vault/${path//\//_}.json" ]] && return 2
                        cat > "${STATE}/vault/${path//\//_}.json" ;;
                    *) return 1 ;;
                esac
            }
            eval "${SCRIPT}"
        ' 2>&1
}

check() {
    if eval "$2"; then printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1))
    else printf '  %sFAIL%s  %s\n%s\n' "${RED}" "${NC}" "$1" "${3:-}"; fail=$((fail + 1)); fi
}
seed() { jq -r .signing_seed "${STATE}/vault/gentian-os_kernel_backup_signing.json" 2>/dev/null; }

echo ""
echo "The bundle signing key in the vault (B-08)"

new_state
out="$(run 'seed_bundle_signing_key')"
first="$(seed)"
check "seeded at gentian-os/kernel/backup/signing: 32 random bytes as hex" '[[ "${first}" =~ ^[0-9a-f]{64}$ ]]' "${out}"
check "the key travels on stdin, never as an argument" '! grep -qE "[0-9a-f]{64}" "${STATE}/bao-args.log"' "$(cat "${STATE}/bao-args.log")"
out="$(run 'seed_bundle_signing_key')"
check "seeded once: a second run leaves the vault's key alone" '[[ "$(seed)" == "${first}" && "${out}" == *"left as it is"* ]]' "${out}"
out="$(run 'seed_bundle_signing_key' BUNDLE_SIGNING_SEED="$(printf 'ab%.0s' {1..32})")"
check "a kit's other key does not replace the vault's, and says so" \
    '[[ "$(seed)" == "${first}" && "${out}" == *"other than the recovery kit"* ]]' "${out}"

new_state
kit="$(printf 'cd%.0s' {1..32})"
out="$(run 'seed_bundle_signing_key' BUNDLE_SIGNING_SEED="${kit}")"
check "a rebuild from the kit seeds the kit's key" '[[ "$(seed)" == "${kit}" && "${out}" == *"from the recovery kit"* ]]' "${out}"
check "the kit's key travels on stdin too" '! grep -q "${kit}" "${STATE}/bao-args.log"'

new_state
out="$(run 'seed_bundle_signing_key' BUNDLE_SIGNING_SEED=not-hex)"
check "a kit value that is no key is not seeded; a random one is" '[[ "$(seed)" =~ ^[0-9a-f]{64}$ && "${out}" == *"not 64 hex"* ]]' "${out}"

new_state
mkdir -p "${STATE}/vault"
printf '{}' > "${STATE}/vault/gentian-os_kernel_backup_signing.json"
out="$(run 'seed_bundle_signing_key; echo "rc=$?"')"
check "a path that exists and cannot be read is not written over" \
    '[[ "$(cat "${STATE}/vault/gentian-os_kernel_backup_signing.json")" == "{}" && "${out}" == *"could not be written"* && "${out}" == *"rc=0"* ]]' "${out}"

echo ""
echo "The recovery kit carries the key"
new_state
printf '{"signing_seed":"%s"}' "${kit}" > "${STATE}/vault/gentian-os_kernel_backup_signing.json"
out="$(run 'printf "%s\n" "${_KIT_KEYS[@]}" | grep -qx BUNDLE_SIGNING_SEED && echo listed
    _kit_backup_identity() { return 0; }
    BAO_TOKEN=t _kit_gather >/dev/null 2>&1; echo "gathered=${BUNDLE_SIGNING_SEED:-}"')"
check "BUNDLE_SIGNING_SEED is one of the kit's keys, read from the vault" \
    '[[ "${out}" == *listed* && "${out}" == *"gathered=${kit}"* ]]' "${out}"

new_state
out="$(run '_kit_backup_identity() { return 0; }
    BAO_TOKEN=t _kit_gather >/dev/null 2>&1; echo "gathered=${BUNDLE_SIGNING_SEED:-}"')"
check "a cluster that has no key yet is given one by the kit export, and the kit carries it" \
    '[[ -n "$(seed)" && "${out}" == *"gathered=$(seed)"* ]]' "${out}"

echo ""
echo "The cluster's id in its claim (step 0)"
claim="${SANDBOX}/cluster.yaml"
printf 'apiVersion: gentianos.io/v1alpha1\nkind: Cluster\nmetadata:\n  name: c1-dev\nspec:\n  kernelDomain: k.example\n  platformRoles:\n    admin: g\n' > "${claim}"
new_state
out="$(run "ensure_claim_cluster_uid '${claim}'")"
uid="$(grep -E '^  clusterUID: ' "${claim}" | awk '{print $2}')"
check "a claim without an id is given one, below kernelDomain" \
    '[[ "${uid}" =~ ^[0-9a-f]{32}$ && "$(grep -A4 "^  kernelDomain:" "${claim}" | tail -1)" == "  clusterUID: ${uid}" && "${out}" == *"was added"* ]]' "${out}"
before="$(cat "${claim}")"
out="$(run "ensure_claim_cluster_uid '${claim}'")"
check "a claim with an id keeps it" '[[ "$(cat "${claim}")" == "${before}" && -z "${out}" ]]' "${out}"
out="$(run "ensure_claim_catalogue_section '${claim}'")"
check "the claim pass every install runs keeps it too" '[[ "$(grep -c "clusterUID:" "${claim}")" == 1 && "$(grep "^  clusterUID:" "${claim}")" == "  clusterUID: ${uid}" ]]' "${out}"
printf 'spec:\n    kernelDomain: k.example\n' > "${SANDBOX}/odd.yaml"
out="$(run "ensure_claim_cluster_uid '${SANDBOX}/odd.yaml'")"
check "a claim laid out otherwise is left alone, and the line to add is named" \
    '! grep -q clusterUID: "${SANDBOX}/odd.yaml" && [[ "${out}" == *"Add it by hand"* ]]' "${out}"
check "a new claim is scaffolded with an id" \
    'grep -q "clusterUID: \$(gentian_new_cluster_uid)" "${REPO}/scripts/lib/bootstrap.sh"'

echo ""
if (( fail > 0 )); then
    printf '%s%d of %d failed.%s\n' "${RED}" "${fail}" "$((pass + fail))" "${NC}"; exit 1
fi
printf '%sAll %d bundle-signing cases pass.%s\n' "${GREEN}" "${pass}" "${NC}"
