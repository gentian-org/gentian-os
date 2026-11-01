#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-log-forwarding.sh
# =============================================================================
# The log forwarder (docs/security/audit-events.md §4) and the alert rules on
# what it counts. The forwarder is the Cluster Composition's; this reads it
# from the render fixtures, which make test-unit-render holds to the
# Composition:
#
#   - a claim with no destination composes no forwarder and no credential;
#     one with a destination composes both, restarted when the token changes;
#   - a destination inside the cluster is trusted by the CA of the
#     certificate the platform had issued for it, and only such a one;
#   - streams names which sinks exist;
#   - the chart version the Composition writes out is versions.yaml's pin;
#   - with Docker, Vector's own test runner holds the transforms to it: an
#     audit line keeps its fields and routes to the audit stream; a credential
#     -- an Authorization field, a bearer, a JWT, a code in a query -- does
#     not leave the node; a text line routes to the operations stream;
#   - with Docker, promtool accepts the audit rules the Composition's
#     monitoring carries.
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
FIXTURES=crossplane/tests/unit/render

# objects <expected.yaml> — what a render fixture composes, as JSON lines, one
# per object: the manifest of a provider-kubernetes Object, else the object.
objects() {
    python3 -c '
import sys, json, yaml
for d in yaml.safe_load_all(open(sys.argv[1])):
    if not d: continue
    m = d.get("spec", {}).get("forProvider", {}).get("manifest") if d.get("kind") == "Object" else None
    print(json.dumps(m or d))' "$1"
}
has() { grep -q "\"kind\": \"$1\".*\"name\": \"$2\"" <<<"$3" || grep -q "\"name\": \"$2\".*\"kind\": \"$1\"" <<<"$3"; }

off="$(objects "${FIXTURES}/cluster-bootstrap/expected.yaml")"
check "no destination: no forwarder" '! has Application log-forwarding "${off}"'
check "no destination: no credential" '! has ExternalSecret log-destination "${off}"'

on="$(objects "${FIXTURES}/cluster-log-forwarding/expected.yaml")"
check "a destination: the forwarder" 'has Application log-forwarding "${on}"'
check "a destination: its credential" 'has ExternalSecret log-destination "${on}"'
check "the endpoint keeps its comma" 'grep -q "ingest?a=1,2" <<<"${on}"'
check "streams [audit]: an audit sink and no operations sink" \
    'grep -q destination_audit <<<"${on}" && ! grep -q destination_operations <<<"${on}"'
check "a new token restarts the forwarder" \
    'grep -q "\"secret.reloader.stakater.com/reload\": \"log-destination\"" <<<"${on}"'

check "an external destination: the forwarder trusts the public roots only" '! grep -q ca_file <<<"${on}"'
inside="$(objects "${FIXTURES}/cluster-log-forwarding-in-cluster/expected.yaml")"
check "a destination in the cluster: the forwarder trusts the certificate the platform had issued for it" \
    'grep -q "\"ca_file\": \"/etc/log-destination-ca/ca.crt\"" <<<"${inside}" && grep -q "\"secretName\": \"log-destination-tls\"" <<<"${inside}"'
check "its issuer signs the platform tenant's Service names, here from the person's own CA" \
    'has ClusterIssuer gentian-ca-platform-services "${inside}" && grep -q "\"secretName\": \"acme-ca\"" <<<"${inside}"'

pin="$(sed -n '/^logForwarding:/,/^$/s/^  chart: "\(.*\)"/\1/p' versions.yaml)"
repo="$(sed -n '/^logForwarding:/,/^$/s/^  repo: \(.*\)/\1/p' versions.yaml)"
check "the Composition's chart is versions.yaml's pin (${pin})" \
    'grep -q "targetRevision: \"${pin}\"" crossplane/compositions/cluster-default.yaml && grep -q "repoURL: ${repo}\$" crossplane/compositions/cluster-default.yaml'

# Monitoring is the same Composition's, and the audit rules ride on it.
check "monitoring off: no stack" '! has Application monitoring "${off}"'
monitoring="$(objects "${FIXTURES}/cluster-monitoring/expected.yaml")"
check "monitoring on: the stack" 'has Application monitoring "${monitoring}"'
check "monitoring: the audit rules" 'grep -q gentian-audit <<<"${monitoring}"'
check "monitoring: the operator's metrics" 'grep -q additionalServiceMonitors <<<"${monitoring}"'
check "monitoring: alerts go where the claim says" 'grep -q "hooks.example.org/alerts" <<<"${monitoring}"'
mpin="$(sed -n '/^monitoring:/,/^$/s/^  chart: "\(.*\)"/\1/p' versions.yaml)"
mrepo="$(sed -n '/^monitoring:/,/^$/s/^  repo: \(.*\)/\1/p' versions.yaml)"
check "the Composition's monitoring chart is versions.yaml's pin (${mpin})" \
    'grep -q "\"targetRevision\": \"${mpin}\"" <<<"${monitoring}" && grep -q "\"repoURL\": \"${mrepo}\"" <<<"${monitoring}"'

printf '%s\n%s\n' "${on}" "${monitoring}" > "${SB}/on.json"
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
vector_image="timberio/vector:$(helm show chart vector/vector --version "${pin}" 2>/dev/null | awk '/^appVersion:/{print $2}')"
if [[ "${vector_image}" != "timberio/vector:" ]] && docker_image "${vector_image}"; then
    mkdir -p "${SB}/secret" && printf x > "${SB}/secret/token"
    out="$(docker run --rm -v "${SB}/vector.yaml:/etc/vector/vector.yaml:ro" -v "${SB}/secret:/etc/log-destination:ro" \
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
