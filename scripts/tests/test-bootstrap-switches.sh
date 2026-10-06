#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-bootstrap-switches.sh
# =============================================================================
# The bootstrap chart has three switches the installer turns on phase by
# phase: the ApplicationSets (C-02), the operator (D-01) and Headlamp's OIDC
# (D-03). The installer passes them with --set-string, so "off" arrives as the
# string "false" -- which a template that tests it for truth reads as ON. A
# fresh install rendered all three in B-01, and Headlamp waited ten minutes on
# a Secret that only D-03 writes.
#
# Rendered exactly as B-01 passes them, with helm and no cluster.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0

render() {
    # The layout and the platform table, as B-01 passes them: the chart
    # names namespaces by function and refuses to render without them.
    helm template boot kernel/bootstrap/chart \
        -f "${NAMESPACES}" -f kernel/platforms.yaml \
        --set-string "appsets.enabled=$1" \
        --set-string "operator.enabled=$2" \
        --set-string "headlamp.oidc.enabled=$3" \
        --set-string "kernelDomain=k.example" \
        --set-string "cluster=c" \
        --set-string "versions.headlamp.chart=0.0.0" \
        --set-string "versions.headlamp.repo=https://example.invalid" "${@:4}" 2>&1
}

has() { grep -qE "$2" <<<"$1"; }

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
NAMESPACES="${TMP}/namespaces.yaml"
{ echo "namespaces:"; sed 's/^/  /' kernel/namespaces.yaml; } > "${NAMESPACES}"

check() {
    local what="$1" ok="$2"
    if [[ "${ok}" == 1 ]]; then
        printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "${what}"; pass=$((pass + 1))
    else
        printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "${what}"; fail=$((fail + 1))
    fi
}

echo ""
echo "Bootstrap chart: the phase switches"
echo ""

off="$(render false false false)"
if has "${off}" '^Error'; then echo "${off}" | head -5; exit 1; fi
check "B-01 (all \"false\"): no gentian-appsets Application" "$(has "${off}" 'name: gentian-appsets$' && echo 0 || echo 1)"
check "B-01 (all \"false\"): no operator Application"         "$(has "${off}" 'name: gentian-os$' && echo 0 || echo 1)"
check "B-01 (all \"false\"): Headlamp does not read headlamp-oidc" "$(has "${off}" 'headlamp-oidc' && echo 0 || echo 1)"
check "B-01 (all \"false\"): no kube-oidc-proxy"              "$(has "${off}" 'name: kube-oidc-proxy$' && echo 0 || echo 1)"

on="$(render true true true)"
check "D-03 (all \"true\"): gentian-appsets Application"      "$(has "${on}" 'name: gentian-appsets$' && echo 1 || echo 0)"
check "D-03 (all \"true\"): operator Application"             "$(has "${on}" 'name: gentian-os$' && echo 1 || echo 0)"
check "D-03 (all \"true\"): Headlamp reads headlamp-oidc"     "$(has "${on}" 'headlamp-oidc' && echo 1 || echo 0)"

# The licence report. The address is the bootstrap chart's default and nobody
# else's; the installer's "off" arrives as the string "false" like every other
# switch, and must reach the operator's chart as a boolean false.
report_on="$(render true true true)"
check "licence report: on by default, at the default address" \
    "$(grep -A2 '^        licenceReport:$' <<<"${report_on}" | grep -q '^          enabled: true$' && has "${report_on}" 'url: "https://corp\.gentian-os\.org/api/v1/licence-reports"$' && echo 1 || echo 0)"
report_off="$(render true true true --set-string licenceReport.enabled=false)"
check "licence report: --no-licence-report renders enabled: false" \
    "$(grep -A2 '^        licenceReport:$' <<<"${report_off}" | grep -q '^          enabled: false$' && echo 1 || echo 0)"
report_else="$(render true true true --set-string licenceReport.url=https://reports.example/r)"
check "licence report: another address replaces the default" \
    "$(has "${report_else}" 'url: "https://reports\.example/r"$' && ! has "${report_else}" 'corp\.gentian-os\.org' && echo 1 || echo 0)"
report_unset="$(render true true true --set-string licenceReport.url=)"
check "licence report: an empty address from the installer is the default" \
    "$(has "${report_unset}" 'url: "https://corp\.gentian-os\.org/api/v1/licence-reports"$' && echo 1 || echo 0)"

echo ""
if (( fail > 0 )); then
    printf '%s%d failed%s, %d passed\n' "${RED}" "${fail}" "${NC}" "${pass}"
    exit 1
fi
printf '%sAll %d switches render as passed.%s\n' "${GREEN}" "${pass}" "${NC}"
