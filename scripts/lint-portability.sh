#!/usr/bin/env bash
# =============================================================================
# scripts/lint-portability.sh — hold the line on macOS compatibility
# =============================================================================
# Bash-4-only constructs are not flagged by shellcheck's defaults, so nothing
# stops a ninth `declare -A` appearing while the known ones are being migrated.
# This does.
#
# It is expected to FAIL until Phase 13 migrates the call sites. A lint that
# passes before the work is done is not enforcing anything — the point is that
# CI reports the exact remaining set, and the number only goes down.
#
# Usage:
#   scripts/lint-portability.sh            report violations, exit 1 if any
#   scripts/lint-portability.sh --count    print the count only
# =============================================================================

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; DIM=$'\033[2m'; NC=$'\033[0m'
COUNT_ONLY=0
[[ "${1:-}" == "--count" ]] && COUNT_ONLY=1

files() {
    git ls-files '*.sh' scripts/kubectl-gentian 2>/dev/null
}

total=0

report() {
    local label="$1" pattern="$2" fix="$3" hits
    hits="$(files | xargs grep -nE "${pattern}" 2>/dev/null |
            grep -vE ':[0-9]+:[[:space:]]*#' || true)"
    local n=0
    [[ -n "${hits}" ]] && n="$(printf '%s\n' "${hits}" | wc -l | tr -d ' ')"
    total=$((total + n))
    [[ ${COUNT_ONLY} -eq 1 ]] && return 0
    if [[ ${n} -eq 0 ]]; then
        printf '  %s✓%s %-34s 0\n' "${GREEN}" "${NC}" "${label}"
    else
        printf '  %s✗%s %-34s %s   %s%s%s\n' "${RED}" "${NC}" "${label}" "${n}" "${DIM}" "${fix}" "${NC}"
        printf '%s\n' "${hits}" | sed 's/^/        /'
    fi
}

[[ ${COUNT_ONLY} -eq 1 ]] || {
    echo ""
    echo "Portability lint — stock macOS bash 3.2 and BSD userland"
    echo ""
}

report "declare -A / local -A"   'declare -A|local -A'          "parallel arrays, or key=value strings"
report "mapfile / readarray"     'mapfile |readarray '          "while IFS= read -r"
# shellcheck disable=SC2016  # the pattern must match a literal ${v^^}
report 'case conversion ${v^^}'  '\$\{[A-Za-z_]+(\^\^|,,)\}'    "to_upper / to_lower from compat.sh"
report "sed -i"                  'sed -i'                       "sed_inplace from compat.sh"
report "xargs -r"                'xargs -r'                     "xargs_r from compat.sh"
report "timeout(1)"              '(^|[;&|(]|\bthen |\bdo )[[:space:]]*timeout[[:space:]]+[0-9]' "the tool's own --timeout flag"

if [[ ${COUNT_ONLY} -eq 1 ]]; then
    echo "${total}"
    exit 0
fi

echo ""
if [[ ${total} -eq 0 ]]; then
    echo "${GREEN}Portable.${NC} The installer runs on stock macOS bash 3.2."
    exit 0
fi
echo "${RED}${total} portability violation(s).${NC}"
echo "Expected to be non-zero until Phase 13 migrates the call sites."
echo "The number must only go down — see docs/plans/config-and-credential-cleanup.md §7."
exit 1
