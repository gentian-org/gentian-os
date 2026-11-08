#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-bootstrap-app-drift.sh
# =============================================================================
# B-01 re-applies the bootstrap Applications when one on the cluster no longer
# holds what the chart renders. It once compared the live object with
# `kubectl apply --dry-run=client -o json` of the rendered file, and for an
# Application that already exists that output carries the live values: a
# record added to certificates.dnsRecords rendered into dns-records, the live
# Application kept `records: []`, and the step reported satisfied.
#
# Held here, against a stand-in kubectl whose dry-run answers as the real one
# did (with the live object):
#
#   - a value changed inside an existing Application is drift;
#   - the same Application holding what the chart renders is not;
#   - a rendered Application missing on the cluster is drift;
#   - the drift script lists the rendered Applications and picks one by name.
#
# No cluster.
# =============================================================================
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

echo ""
echo "bootstrap drift: a value changed in an existing Application is drift"
echo ""

if ! python3 -c 'import yaml' 2>/dev/null; then
    echo "  skipped: python3 with PyYAML is not on this host"
    exit 0
fi

SB="$(mktemp -d "${TMPDIR:-/tmp}/gbd.XXXXXX")"
trap 'rm -rf "${SB}"' EXIT
mkdir -p "${SB}/bin" "${SB}/home"

# What the chart renders: two Applications and something that is not one.
cat >"${SB}/rendered.yaml" <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: kernel-edge
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: external-dns
  namespace: kernel-gitops
spec:
  project: gentian
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: dns-records
  namespace: kernel-gitops
spec:
  project: gentian
  source:
    helm:
      valuesObject:
        records: [{"name":"docs.example.org","target":"example.github.io","type":"CNAME"}]
YAML

live_app() {  # live_app <name> <records-json>
    printf '{"apiVersion":"argoproj.io/v1alpha1","kind":"Application","metadata":{"name":"%s","namespace":"kernel-gitops","generation":7},"spec":{"project":"gentian","source":{"helm":{"valuesObject":{"records":%s}}}}}' "$1" "$2"
}

# get application <name> -o json answers from ${SB}/live/<name>.json (absent:
# not found). A client dry-run answers with the live objects, as kubectl does
# for objects that exist.
cat >"${SB}/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
all="$*"
case "${all}" in
    *"get application "*)
        name="$(awk '{for (i = 1; i <= NF; i++) if ($i == "application") print $(i + 1)}' <<<"${all}")"
        [[ -f "${LIVE}/${name}.json" ]] || exit 1
        cat "${LIVE}/${name}.json" ;;
    *"apply --dry-run=client"*)
        jq -s '{kind: "List", items: .}' "${LIVE}"/*.json ;;
esac
exit 0
STUB
chmod +x "${SB}/bin/kubectl"

# verdict — _v5_bootstrap_apps_current over the fixture, as B-01 loads it.
verdict() {
    env -i HOME="${SB}/home" PATH="${SB}/bin:${PATH}" SCRIPT_DIR="$(pwd)" LIVE="${SB}/live" RENDERED="${SB}/rendered.yaml" \
        bash -c 'set -u; source scripts/lib/load.sh >/dev/null 2>&1; trap - ERR; set +e
source scripts/steps/B-01-bootstrap-apps.sh
_v5_render() { cat "${RENDERED}"; }
_v5_bootstrap_apps_current; echo "rc=$?"' 2>&1 | tail -1
}

RECORD='[{"name":"docs.example.org","target":"example.github.io","type":"CNAME"}]'

mkdir -p "${SB}/live"
live_app external-dns '[]' >"${SB}/live/external-dns.json"
live_app dns-records '[]' >"${SB}/live/dns-records.json"
out="$(verdict)"
check "a record rendered into dns-records, live still empty: drift" '[[ "${out}" == "rc=1" ]]' "${out}"

live_app dns-records "${RECORD}" >"${SB}/live/dns-records.json"
out="$(verdict)"
check "dns-records live as rendered: current" '[[ "${out}" == "rc=0" ]]' "${out}"

rm "${SB}/live/dns-records.json"
out="$(verdict)"
check "dns-records rendered and not on the cluster: drift" '[[ "${out}" == "rc=1" ]]' "${out}"

out="$(python3 scripts/lib/bootstrap-app-drift.py --names "${SB}/rendered.yaml" | tr '\n' ' ')"
check "--names lists the rendered Applications only" '[[ "${out}" == "external-dns dns-records " ]]' "${out}"

live_app dns-records '[]' >"${SB}/live/dns-records.json"
python3 scripts/lib/bootstrap-app-drift.py "${SB}/rendered.yaml" "${SB}/live/dns-records.json" dns-records >/dev/null; rc=$?
check "a named Application is compared with its own live object" '[[ "${rc}" == 1 ]]' "rc=${rc}"
python3 scripts/lib/bootstrap-app-drift.py "${SB}/rendered.yaml" "${SB}/live/dns-records.json" >/dev/null 2>&1; rc=$?
check "several rendered Applications and no name: cannot tell" '[[ "${rc}" == 2 ]]' "rc=${rc}"

finish
