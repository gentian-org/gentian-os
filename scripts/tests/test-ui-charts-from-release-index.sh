#!/usr/bin/env bash
# shellcheck disable=SC1090,SC1091,SC2015,SC2329 # sourced lib and step functions; ok/bad report, never fail the line; stand-ins called by the sourced functions
# D-03 renders a gentian-ui chart at the version the release index lists for
# the release the claim names, because the operator's release reconciler sets
# exactly that one (os #1387); a chart the entry does not list, and every
# chart on a track, is the branch's immutable build as before.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail=0
ok()  { echo "  ok   $*"; }
bad() { echo "  FAIL $*"; fail=1; }

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
export GENTIAN_DEPLOYMENTS_PATH="${work}/deployments" GENTIAN_DEPLOYMENTS_CLUSTER_ID=c1
claim="${GENTIAN_DEPLOYMENTS_PATH}/clusters/c1/kernel/claims/cluster.yaml"
mkdir -p "$(dirname "${claim}")"

cat >"${work}/index.yaml" <<'EOF'
releases:
  - version: v0.5.1
    line: "0.5"
  - version: v0.5.2
    line: "0.5"
    ui:
      desktop: 0.1.0-v0.5.2
      adminConsole: 0.1.1-v0.5.2
EOF

for fn in yq_get cluster_claim_path claim_release_ref; do
    eval "$(sed -n "/^${fn}() {/,/^}/p" "${ROOT}/scripts/lib/common.sh")"
done
step="${ROOT}/scripts/steps/D-03-portal-login.sh"
source <(sed -n '/^_D03_RELEASE_FEED_DEFAULT=/,/^_d02_export_layout()/p' "${step}" | sed '$d')
error() { :; }

# Stand-ins: the feed is the fixture above (or unreachable), and the branch's
# immutable build is named by its arguments.
fetched=0
curl() {
    fetched=$((fetched + 1))
    [[ -z "${STANDIN_FEED_DOWN:-}" ]] || return 22
    local out=""
    while [[ $# -gt 0 ]]; do [[ "$1" == "-o" ]] && out="$2"; shift; done
    cp "${work}/index.yaml" "${out}"
}
_d02_component_chart_version() { echo "build:$1:$2"; }
gentian_ui_chart_branch() { claim_release_ref; }

ui() { _d03_ui_chart_version "$1" "repo/$1" "$2" "${work}/fetched"; }

printf 'spec:\n  release:\n    version: v0.5.2\n' >"${claim}"
: >"${work}/fetched"; fetched=0
_d03_release_index "${work}/fetched" && ok "release: the index is read" || bad "release: the index was not read"
[[ "$(ui desktop 0.1.0)" == "0.1.0-v0.5.2" ]] && ok "release: desktop from the index" || bad "release: desktop is $(ui desktop 0.1.0)"
[[ "$(ui adminConsole 0.1.1)" == "0.1.1-v0.5.2" ]] && ok "release: admin console from the index" || bad "release: admin console is $(ui adminConsole 0.1.1)"
[[ "$(ui concierge 0.1.0)" == "build:repo/concierge:0.1.0-v0.5.2" ]] && ok "release: a chart the entry does not list is the build" || bad "release: concierge is $(ui concierge 0.1.0)"

printf 'spec:\n  release:\n    version: v0.5.1\n' >"${claim}"
[[ "$(ui desktop 0.1.0)" == "build:repo/desktop:0.1.0-v0.5.1" ]] && ok "release without ui: the build" || bad "release without ui: desktop is $(ui desktop 0.1.0)"

printf 'spec:\n  release:\n    track: develop\n' >"${claim}"
: >"${work}/fetched"; fetched=0
_d03_release_index "${work}/fetched" && [[ "${fetched}" == 0 && ! -s "${work}/fetched" ]] \
    && ok "track: the index is not read" || bad "track: the index was read (${fetched})"
[[ "$(ui desktop 0.1.0)" == "build:repo/desktop:0.1.0-develop" ]] && ok "track: the build" || bad "track: desktop is $(ui desktop 0.1.0)"

printf 'spec:\n  release:\n    version: v0.5.2\n' >"${claim}"
STANDIN_FEED_DOWN=1 _d03_release_index "${work}/fetched" && bad "an unreadable index passes" || ok "an unreadable index stops the step"

exit "${fail}"
