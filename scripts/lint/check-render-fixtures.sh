#!/usr/bin/env bash
# =============================================================================
# scripts/lint/check-render-fixtures.sh — render tests must test the real thing
# =============================================================================
# Each crossplane/tests/unit/render/*/ directory holds its own copy of the
# Composition under test. A copy can drift from the original, and when it does
# the golden test keeps passing against a Composition that is no longer
# deployed — false confidence, which is worse than no test.
#
# This was not hypothetical: cluster-bootstrap's copy went stale the moment
# cluster-default.yaml was edited, and the suite stayed green.
# =============================================================================

set -euo pipefail
# scripts/lint/ -> repo root
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; NC=$'\033[0m'
stale=0

for dir in crossplane/tests/unit/render/*/; do
    copy="${dir}composition.yaml"
    [[ -f "${copy}" ]] || continue
    name="$(grep -m1 '^  name:' "${copy}" | awk '{print $2}')"
    real="crossplane/compositions/${name}.yaml"
    [[ -f "${real}" ]] || continue

    if diff -q "${real}" "${copy}" >/dev/null 2>&1; then
        printf '  %s✓%s %-28s %s\n' "${GREEN}" "${NC}" "$(basename "${dir}")" "${name}"
    else
        printf '  %s✗%s %-28s %s — copy differs from the deployed Composition\n' \
            "${RED}" "${NC}" "$(basename "${dir}")" "${name}"
        printf '        cp %s %s\n' "${real}" "${copy}"
        stale=1
    fi
done

[[ ${stale} -eq 0 ]] || { echo ""; echo "${RED}Stale render fixture(s).${NC}"; exit 1; }
