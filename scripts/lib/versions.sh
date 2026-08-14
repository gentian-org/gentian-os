#!/usr/bin/env bash
# =============================================================================
# scripts/lib/versions.sh — read component pins from versions.yaml
# =============================================================================
# Dual-mode. Source it:
#
#   source scripts/lib/versions.sh
#   gentian_pin crossplane chart
#
# or execute it, which is what the Makefile and standalone scripts do:
#
#   bash scripts/lib/versions.sh crossplane cli
#
# Deliberately self-contained: no colour helpers, no scripts/lib/common.sh, no
# SCRIPT_DIR from the caller. That is the whole point — scripts/install-argocd.sh
# and the Makefile need pins without loading the install library, and a
# dependency here would put the version inventory behind the very thing it
# configures.
#
# It carries its own flavor-tolerant yq read rather than reusing common.sh's
# yq_get for the same reason. Ten duplicated lines is the price of a file with
# no prerequisites.
# =============================================================================

[[ -n "${GENTIAN_VERSIONS_LOADED:-}" ]] && return 0 2>/dev/null
GENTIAN_VERSIONS_LOADED=1

# Resolve the repo root from this file, not from the caller's cwd.
GENTIAN_VERSIONS_FILE="${GENTIAN_VERSIONS_FILE:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/versions.yaml}"

# _gentian_yq <filter> <file> — tolerate either yq flavor (mikefarah's `eval`
# subcommand, or kislyuk's jq-style filter); both are seen in the wild as `yq`.
_gentian_yq() {
    local filter="$1" file="$2" out
    if out=$(yq eval "${filter}" "${file}" 2>/dev/null) && [[ -n "${out}" && "${out}" != "null" ]]; then
        echo "${out}"; return 0
    fi
    if out=$(yq -r "${filter}" "${file}" 2>/dev/null) && [[ -n "${out}" && "${out}" != "null" ]]; then
        echo "${out}"; return 0
    fi
    return 1
}

# gentian_pin <component> <field> — echo the pin, or fail loudly.
#
# Fails rather than defaulting on purpose. A missing pin that silently falls
# back to "latest" is the failure mode this file exists to remove: it produces
# two clusters running different versions with nothing recording which.
gentian_pin() {
    local component="${1:-}" field="${2:-}" value
    if [[ -z "${component}" || -z "${field}" ]]; then
        echo "gentian_pin: need <component> <field>" >&2
        return 2
    fi
    if [[ ! -f "${GENTIAN_VERSIONS_FILE}" ]]; then
        echo "versions.yaml not found at ${GENTIAN_VERSIONS_FILE}" >&2
        return 1
    fi
    if ! value="$(_gentian_yq ".\"${component}\".${field}" "${GENTIAN_VERSIONS_FILE}")"; then
        echo "No pin for '${component}.${field}' in ${GENTIAN_VERSIONS_FILE}" >&2
        return 1
    fi
    echo "${value}"
}

# gentian_pin_components — every component name, for the mirror inventory and
# for the `# pins:` lint.
gentian_pin_components() {
    _gentian_yq 'keys | .[]' "${GENTIAN_VERSIONS_FILE}" 2>/dev/null ||
        _gentian_yq 'keys[]' "${GENTIAN_VERSIONS_FILE}" 2>/dev/null
}

# Executed rather than sourced: print one pin and exit.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    if [[ $# -lt 2 ]]; then
        echo "usage: $0 <component> <field>" >&2
        echo "       $0 --components" >&2
        [[ "${1:-}" == "--components" ]] && { gentian_pin_components; exit 0; }
        exit 1
    fi
    gentian_pin "$1" "$2"
fi
