#!/usr/bin/env bash
# =============================================================================
# scripts/lint/check-render-fixtures.sh — render tests must test the real thing
# =============================================================================
# Each crossplane/tests/unit/render/*/ directory's composition.yaml is a
# symlink to the deployed Composition under crossplane/compositions/, so the
# golden test always renders what the cluster runs and no copy can drift.
#
# A fixture fails when its composition.yaml is missing, is a regular file (a
# copy), or is a link that does not resolve to a file in crossplane/compositions/
# (a Composition renamed or removed). make test-unit-render skips a directory
# without a composition.yaml, so without this check a fixture whose Composition
# is gone would drop out of the suite silently.
#
# _harness is the one exception: it tests the render harness itself with a
# Composition of its own that is deployed nowhere.
# =============================================================================

set -euo pipefail
# scripts/lint/ -> repo root
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; NC=$'\033[0m'
compositions="$(cd crossplane/compositions && pwd -P)"
bad=0

fail() {
    printf '  %s✗%s %-36s %s\n' "${RED}" "${NC}" "$1" "$2"
    bad=1
}

for dir in crossplane/tests/unit/render/*/; do
    case="$(basename "${dir}")"
    link="${dir}composition.yaml"
    [[ "${case}" == "_harness" ]] && continue

    if [[ ! -L "${link}" ]]; then
        if [[ -e "${link}" ]]; then
            fail "${case}" "composition.yaml is a copy — link it to crossplane/compositions/"
        else
            fail "${case}" "no composition.yaml — the fixture names no Composition"
        fi
        continue
    fi
    points_to="$(readlink "${link}")"
    if [[ ! -f "${link}" ]]; then
        fail "${case}" "composition.yaml -> ${points_to}, which does not exist"
        continue
    fi
    target_dir="$(cd "${dir}" && cd "$(dirname "${points_to}")" && pwd -P)"
    if [[ "${target_dir}" != "${compositions}" ]]; then
        fail "${case}" "composition.yaml -> ${points_to}, which is outside crossplane/compositions/"
        continue
    fi
    printf '  %s✓%s %-36s %s\n' "${GREEN}" "${NC}" "${case}" "$(basename "${points_to}" .yaml)"
done

[[ ${bad} -eq 0 ]] || { echo ""; echo "${RED}Render fixture(s) without a deployed Composition.${NC}"; exit 1; }
