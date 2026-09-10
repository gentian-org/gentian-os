#!/usr/bin/env bash
# =============================================================================
# scripts/check-credentials.sh — is every declared credential satisfied?
# =============================================================================
# One implementation, three callers:
#
#   --source=vault    installer preflight, before the cluster has ESO. Asks
#                     OpenBao directly whether each path exists.
#   --source=cluster  day-2 report. Reads the ExternalSecret probes ESO
#                     maintains; touches OpenBao not at all.
#   --source=git      CI on gentian-deployments. Reads the Repository claims a
#                     branch declares and checks their paths, so a pull request
#                     adding a private repository fails review instead of
#                     merging cleanly and surfacing an hour later as a stuck
#                     sync.
#
# The git mode is why this is a script rather than a function: CI has no
# installer, no kubeconfig, and no business loading scripts/lib/load.sh.
#
# METADATA ONLY. Every mode asks whether a path exists and never reads a value.
# The OpenBao policy this needs is `list` on the metadata path — a CI job that
# could read secrets would be a worse problem than the one it prevents.
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CATALOGUE="${GENTIAN_CATALOGUE_FILE:-${SCRIPT_DIR}/credentials.yaml}"

SOURCE="cluster"
PHASE_FILTER=""
CLAIMS_DIR=""
QUIET=0

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; DIM=$'\033[2m'; NC=$'\033[0m'

usage() {
    sed -n '3,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    cat <<'EOF'

Options:
  --source=vault|cluster|git   where to look (default: cluster)
  --phase=bootstrap|runtime    only check this phase
  --claims-dir=PATH            git mode: directory of Repository claims
  --quiet                      only report failures
  -h, --help
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --source=*)     SOURCE="${1#*=}" ;;
        --phase=*)      PHASE_FILTER="${1#*=}" ;;
        --claims-dir=*) CLAIMS_DIR="${1#*=}" ;;
        --quiet)        QUIET=1 ;;
        -h|--help)      usage; exit 0 ;;
        *) echo "Unknown option: $1" >&2; usage; exit 1 ;;
    esac
    shift
done

_yq() {
    local out
    if out=$(yq eval "$1" "$2" 2>/dev/null) && [[ "${out}" != "null" ]]; then echo "${out}"; return 0; fi
    if out=$(yq -r "$1" "$2" 2>/dev/null) && [[ "${out}" != "null" ]]; then echo "${out}"; return 0; fi
    return 1
}

SATISFIED=0; MISSING=0; SKIPPED=0
declare -a UNSATISFIED_REQUIRED=()

_report() {
    local name="$1" state="$2" detail="${3:-}"
    case "${state}" in
        satisfied)
            SATISFIED=$((SATISFIED + 1))
            [[ ${QUIET} -eq 1 ]] || printf '  %s✓%s %-28s %s\n' "${GREEN}" "${NC}" "${name}" "${DIM}${detail}${NC}"
            ;;
        optional-missing)
            SKIPPED=$((SKIPPED + 1))
            [[ ${QUIET} -eq 1 ]] || printf '  %s○%s %-28s %s\n' "${YELLOW}" "${NC}" "${name}" "${DIM}not set (optional)${NC}"
            ;;
        missing)
            MISSING=$((MISSING + 1))
            UNSATISFIED_REQUIRED+=("${name}")
            printf '  %s✗%s %-28s %s\n' "${RED}" "${NC}" "${name}" "${detail}"
            ;;
    esac
}

# =============================================================================
# vault — installer preflight
# =============================================================================
check_via_vault() {
    command -v bao >/dev/null 2>&1 || { echo "bao CLI not found" >&2; exit 1; }
    local name path optional
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        path="$(_yq ".requirements[] | select(.name == \"${name}\") | .vaultPath" "${CATALOGUE}")"
        optional="$(_yq ".requirements[] | select(.name == \"${name}\") | .optional" "${CATALOGUE}" || echo false)"
        # Metadata read only — never `bao kv get`, which returns the value.
        if bao kv metadata get -mount=secret "${path}" >/dev/null 2>&1; then
            _report "${name}" satisfied "${path}"
        elif [[ "${optional}" == "true" ]]; then
            _report "${name}" optional-missing
        else
            _report "${name}" missing "no value at ${path}"
        fi
    done < <(_list_names)
}

# =============================================================================
# cluster — read what ESO already knows
# =============================================================================
check_via_cluster() {
    command -v kubectl >/dev/null 2>&1 || { echo "kubectl not found" >&2; exit 1; }
    local name es ready optional
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        es="credreq-${name}"
        optional="$(_yq ".requirements[] | select(.name == \"${name}\") | .optional" "${CATALOGUE}" || echo false)"

        if ! kubectl get externalsecret "${es}" -n gentian-system >/dev/null 2>&1; then
            _report "${name}" missing "no probe ExternalSecret ${es} — is the catalogue applied?"
            continue
        fi
        ready="$(kubectl get externalsecret "${es}" -n gentian-system \
            -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
        if [[ "${ready}" == "True" ]]; then
            _report "${name}" satisfied "ESO reports Ready"
        elif [[ "${optional}" == "true" ]]; then
            _report "${name}" optional-missing
        else
            _report "${name}" missing "$(kubectl get externalsecret "${es}" -n gentian-system \
                -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || echo 'not Ready')"
        fi
    done < <(_list_names)
}

# =============================================================================
# git — CI on a deployments branch
# =============================================================================
check_via_git() {
    [[ -n "${CLAIMS_DIR}" ]] || { echo "--claims-dir is required for --source=git" >&2; exit 1; }
    [[ -d "${CLAIMS_DIR}" ]] || { echo "No such directory: ${CLAIMS_DIR}" >&2; exit 1; }

    local f name path optional found=0
    while IFS= read -r f; do
        [[ -n "${f}" ]] || continue
        [[ "$(_yq '.kind' "${f}" || true)" == "Repository" ]] || continue
        found=1
        name="$(_yq '.metadata.name' "${f}")"
        path="$(_yq '.spec.credential.vaultPath' "${f}" || true)"
        optional="$(_yq '.spec.credential.optional' "${f}" || echo false)"

        if [[ -z "${path}" ]]; then
            _report "${name}" missing "claim declares no credential.vaultPath"
            continue
        fi
        if bao kv metadata get -mount=secret "${path}" >/dev/null 2>&1; then
            _report "${name}" satisfied "${path}"
        elif [[ "${optional}" == "true" ]]; then
            _report "${name}" optional-missing
        else
            _report "${name}" missing "this change needs ${path}, which is not set"
        fi
    done < <(find "${CLAIMS_DIR}" -name '*.yaml' -o -name '*.yml' | sort)

    [[ ${found} -eq 1 ]] || echo "  no Repository claims found in ${CLAIMS_DIR}"
}

_list_names() {
    if [[ -n "${PHASE_FILTER}" ]]; then
        _yq ".requirements[] | select(.phase == \"${PHASE_FILTER}\") | .name" "${CATALOGUE}"
    else
        _yq '.requirements[].name' "${CATALOGUE}"
    fi
}

# =============================================================================
main() {
    echo ""
    echo "Credential check — source: ${SOURCE}${PHASE_FILTER:+, phase: ${PHASE_FILTER}}"
    echo ""

    case "${SOURCE}" in
        vault)   check_via_vault ;;
        cluster) check_via_cluster ;;
        git)     check_via_git ;;
        *) echo "Unknown source: ${SOURCE}" >&2; exit 1 ;;
    esac

    echo ""
    printf 'satisfied: %d   optional unset: %d   missing: %d\n' \
        "${SATISFIED}" "${SKIPPED}" "${MISSING}"

    if [[ ${MISSING} -gt 0 ]]; then
        echo ""
        echo "${RED}Unsatisfied required credentials:${NC} ${UNSATISFIED_REQUIRED[*]}"
        echo "Supply them through the credential manager, or with the installer for phase: bootstrap."
        exit 1
    fi
    exit 0
}

main
