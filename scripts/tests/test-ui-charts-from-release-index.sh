#!/usr/bin/env bash
# shellcheck disable=SC1090,SC1091,SC2015,SC2329 # sourced lib and step functions; ok/bad report, never fail the line; stand-ins called by the sourced functions
# D-03 renders a gentian-ui chart at the version the release index lists for
# the release the claim names, because the operator's release reconciler sets
# exactly that one (os #1387). One the entry does not list is the release's
# number; the charts published with the operator's image are left to the
# operator chart on a release; and on a track every chart is its newest build
# of a branch, the highest by semver among the versions the registry lists.
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
source <(sed -n '/^_d03_newest_build()/,/^_d02_export_layout()/p' "${step}" | sed '$d')
error() { :; }

# Stand-ins: the feed is the fixture above (or unreachable), and the newest
# build of a chart is named by its repository.
fetched=0
curl() {
    fetched=$((fetched + 1))
    [[ -z "${STANDIN_FEED_DOWN:-}" ]] || return 22
    local out=""
    while [[ $# -gt 0 ]]; do [[ "$1" == "-o" ]] && out="$2"; shift; done
    cp "${work}/index.yaml" "${out}"
}
_d03_newest_chart_build() { echo "build:$1"; }
gentian_os_ref() { claim_release_ref; }

chart() { _d03_chart_version "repo/$1" "${work}/fetched" "${2:-}"; }

printf 'spec:\n  release:\n    version: v0.5.2\n' >"${claim}"
: >"${work}/fetched"; fetched=0
_d03_release_index "${work}/fetched" && ok "release: the index is read" || bad "release: the index was not read"
[[ "$(chart desktop desktop)" == "0.1.0-v0.5.2" ]] && ok "release: desktop from the index" || bad "release: desktop is $(chart desktop desktop)"
[[ "$(chart admin-console adminConsole)" == "0.1.1-v0.5.2" ]] && ok "release: admin console from the index" || bad "release: admin console is $(chart admin-console adminConsole)"
[[ "$(chart concierge concierge)" == "0.5.2" ]] && ok "release: a chart the entry does not list is the release's number" || bad "release: concierge is $(chart concierge concierge)"
[[ -z "$(chart homesync)" ]] && ok "release: a chart published with the operator's image is left to the operator chart" || bad "release: homesync is $(chart homesync)"

printf 'spec:\n  release:\n    version: v0.5.1\n' >"${claim}"
[[ "$(chart desktop desktop)" == "0.5.1" ]] && ok "release without ui: the release's number" || bad "release without ui: desktop is $(chart desktop desktop)"

printf 'spec:\n  release:\n    track: develop\n' >"${claim}"
: >"${work}/fetched"; fetched=0
_d03_release_index "${work}/fetched" && [[ "${fetched}" == 0 && ! -s "${work}/fetched" ]] \
    && ok "track: the index is not read" || bad "track: the index was read (${fetched})"
[[ "$(chart desktop desktop)" == "build:repo/desktop" ]] && ok "track: the newest build" || bad "track: desktop is $(chart desktop desktop)"
[[ "$(chart homesync)" == "build:repo/homesync" ]] && ok "track: the newest build of the operator's own charts too" || bad "track: homesync is $(chart homesync)"

printf 'spec:\n  release:\n    version: v0.5.2\n' >"${claim}"
STANDIN_FEED_DOWN=1 _d03_release_index "${work}/fetched" && bad "an unreadable index passes" || ok "an unreadable index stops the step"

# The newest build among a chart's versions: builds of a branch only, by the
# release they lead up to and then by their number, as numbers.
got="$(printf '%s\n' 0.1.0-develop 0.1.0-develop.89abcde 0.1.0-v0.5.2 0.5.2 0.5.3 \
    0.5.3-dev.9.gaaaaaaa 0.5.3-dev.812.g0123456 0.5.3-dev.90.gbbbbbbb 0.5.4-dev.3.gccccccc 0.5.4-dev.3 sha256-abc.sig | _d03_newest_build)"
[[ "${got}" == "0.5.4-dev.3.gccccccc" ]] && ok "the newest build is the highest by semver" || bad "the newest build is ${got}"
got="$(printf '%s\n' 0.5.3-dev.9.gaaaaaaa 0.5.3-dev.812.g0123456 0.5.3-dev.90.gbbbbbbb | _d03_newest_build)"
[[ "${got}" == "0.5.3-dev.812.g0123456" ]] && ok "build numbers compare as numbers" || bad "the newest build is ${got}"
[[ -z "$(printf '%s\n' 0.5.3 0.1.0-develop.89abcde | _d03_newest_build)" ]] && ok "a release and a version of the old scheme are no builds" || bad "a release counted as a build"

exit "${fail}"
