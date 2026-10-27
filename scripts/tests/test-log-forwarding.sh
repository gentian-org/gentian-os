#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-log-forwarding.sh
# =============================================================================
# The log forwarder (docs/design/audit-events.md §4) and the alert rules on
# what it counts. Held here, from the bootstrap chart as B-01 renders it:
#
#   - no destination on the claim renders no forwarder, no credential and no
#     audit rules; a destination renders all three;
#   - streams names which sinks exist;
#   - with Docker, Vector's own test runner holds the transforms to it: an
#     audit line keeps its fields and routes to the audit stream; a credential
#     -- an Authorization field, a bearer, a JWT, a code in a query -- does
#     not leave the node; a text line routes to the operations stream;
#   - with Docker, promtool accepts the rules.
#
# Without Docker the last two are skipped and said to be; in CI the images
# are pulled.
# =============================================================================
# shellcheck disable=SC2016,SC2034
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
pass=0; fail=0
ok()  { printf '  %sok%s    %s\n' "${GREEN}" "${NC}" "$1"; pass=$((pass + 1)); }
bad() { printf '  %sFAIL%s  %s\n' "${RED}" "${NC}" "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2"; fail=$((fail + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1" "${3:-}"; fi; }

echo ""
echo "log forwarding: rendered where a destination is named, and nothing leaves a node that should not"
echo ""
for tool in helm python3; do
    command -v "${tool}" >/dev/null 2>&1 || { echo "  ${YELLOW}skipped${NC}: ${tool} is not installed."; exit 0; }
done
SB="$(mktemp -d "${TMPDIR:-/tmp}/glf.XXXXXX")"
trap 'rm -rf "${SB}"' EXIT
{ echo "namespaces:"; sed 's/^/  /' kernel/namespaces.yaml; } > "${SB}/ns.yaml"

# render <values-file|-> [--set-string ...] — the bootstrap chart as JSON lines,
# one object per line.
render() {
    local values="$1"; shift
    local args=(-f "${SB}/ns.yaml" -f kernel/platforms.yaml --set-string kernelDomain=k.example
        --set-string "versions.logForwarding.chart=$(sed -n '/^logForwarding:/,/^$/s/^  chart: "\(.*\)"/\1/p' versions.yaml)"
        --set-string versions.logForwarding.repo=https://helm.vector.dev)
    [[ "${values}" != "-" ]] && args+=(-f "${values}")
    helm template b kernel/bootstrap/chart "${args[@]}" "$@" 2>&1 | python3 -c '
import sys, json, yaml
for d in yaml.safe_load_all(sys.stdin):
    if d: print(json.dumps(d))'
}
has() { grep -q "\"kind\": \"$1\".*\"name\": \"$2\"" <<<"$3" || grep -q "\"name\": \"$2\".*\"kind\": \"$1\"" <<<"$3"; }

off="$(render - --set-string monitoring.enabled=true)"
check "no destination: no forwarder" '! has Application log-forwarding "${off}"'
check "no destination: no credential" '! has ExternalSecret log-destination "${off}"'
check "no destination: no audit rules" '! grep -q gentian-audit <<<"${off}"'

printf 'logForwarding:\n  endpoint: "https://evidence.example.org/ingest?a=1,2"\n' > "${SB}/on.yaml"
on="$(render "${SB}/on.yaml" --set-string monitoring.enabled=true)"
check "a destination: the forwarder" 'has Application log-forwarding "${on}"'
check "a destination: its credential" 'has ExternalSecret log-destination "${on}"'
check "a destination with monitoring: the audit rules" 'grep -q gentian-audit <<<"${on}"'
check "the endpoint keeps its comma" 'grep -q "ingest?a=1,2" <<<"${on}"'

printf 'logForwarding:\n  endpoint: "https://e.example.org/i"\n  streams: [audit]\n' > "${SB}/audit.yaml"
only="$(render "${SB}/audit.yaml")"
check "streams [audit]: an audit sink and no operations sink" \
    'grep -q destination_audit <<<"${only}" && ! grep -q destination_operations <<<"${only}"'

printf '%s\n' "${on}" > "${SB}/on.json"
python3 - "${SB}" <<'PY'
import json, sys, yaml
sb = sys.argv[1]
for line in open(f"{sb}/on.json"):
    d = json.loads(line)
    if d.get("kind") == "Application" and d["metadata"]["name"] == "log-forwarding":
        cfg = d["spec"]["source"]["helm"]["valuesObject"]["customConfig"]
        cfg["tests"] = [
            {"name": "audit line: fields kept, credentials gone, audit stream",
             "inputs": [{"insert_at": "parsed", "type": "log", "log_fields": {
                 "message": json.dumps({"msg": "audit", "audit": True, "source": "bouncer", "kind": "decision",
                                        "outcome": "refused", "subject": "2f2a", "object": "app:acme/x",
                                        "authorization": "Bearer sekrit-a",
                                        "detail": {"note": "Bearer sekrit-b", "jwt": "eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.c2VrcmV0LXNpZ25h"}}),
                 "kubernetes": {"pod_namespace": "kernel-edge", "pod_name": "b", "container_name": "c"}}}],
             "outputs": [{"extract_from": "streams.audit", "conditions": [{"type": "vrl", "source":
                 'assert!(.audit == true)\nassert!(.source == "bouncer")\nassert!(!exists(.authorization))\n'
                 'assert!(!contains(encode_json(.), "sekrit"))\nassert!(!contains(encode_json(.), "eyJhbGciOiJIUzI1"))\n'
                 'assert!(.cluster_namespace == "kernel-edge")'}]}]},
            {"name": "text line: a code in a query redacted, operations stream",
             "inputs": [{"insert_at": "parsed", "type": "log", "log_fields": {
                 "message": "GET /callback?code=sekrit-code&state=1 200",
                 "kubernetes": {"pod_namespace": "tenant-acme", "pod_name": "a", "container_name": "a"}}}],
             "outputs": [{"extract_from": "streams._unmatched", "conditions": [{"type": "vrl", "source":
                 'assert!(!contains(string!(.message), "sekrit-code"))'}]}]},
        ]
        yaml.safe_dump(cfg, open(f"{sb}/vector.yaml", "w"))
    if d.get("kind") == "Application" and d["metadata"]["name"] == "monitoring":
        rules = d["spec"]["source"]["helm"]["valuesObject"].get("additionalPrometheusRulesMap", {}).get("gentian-audit")
        if rules:
            yaml.safe_dump(rules, open(f"{sb}/rules.yaml", "w"))
PY

docker_image() {
    local image="$1"
    command -v docker >/dev/null 2>&1 || return 1
    docker image inspect "${image}" >/dev/null 2>&1 && return 0
    [[ -n "${CI:-}" ]] && docker pull -q "${image}" >/dev/null 2>&1
}
vector_image="timberio/vector:$(helm show chart vector/vector --version "$(sed -n '/^logForwarding:/,/^$/s/^  chart: "\(.*\)"/\1/p' versions.yaml)" 2>/dev/null | awk '/^appVersion:/{print $2}')"
if [[ "${vector_image}" != "timberio/vector:" ]] && docker_image "${vector_image}"; then
    out="$(docker run --rm -e LOG_DESTINATION_TOKEN=x -v "${SB}/vector.yaml:/etc/vector/vector.yaml:ro" \
        "${vector_image}" test /etc/vector/vector.yaml 2>&1)"
    check "Vector's own tests of the transforms pass" '[[ "${out}" != *failed* && "${out}" == *passed* ]]' "${out}"
else
    echo "  ${YELLOW}skipped${NC}: the Vector image is not here (helm repo add vector https://helm.vector.dev; docker pull ${vector_image})."
fi
if [[ -s "${SB}/rules.yaml" ]] && docker_image prom/prometheus:v3.5.0; then
    out="$(docker run --rm --entrypoint promtool -v "${SB}/rules.yaml:/rules.yaml:ro" prom/prometheus:v3.5.0 check rules /rules.yaml 2>&1)"
    check "promtool accepts the audit rules" '[[ "${out}" == *SUCCESS* ]]' "${out}"
else
    echo "  ${YELLOW}skipped${NC}: promtool's image is not here (docker pull prom/prometheus:v3.5.0)."
fi

echo ""
echo "  ${pass} passed, ${fail} failed"
[[ ${fail} -eq 0 ]]
