#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-lb-annotations.sh
# =============================================================================
# The claim's lbAnnotations, platformParams and dnsParams reach Helm through
# gentian_set_args_from_pairs as --set-string arguments. An annotation key has
# dots (loadbalancer.openstack.org/...), which Helm reads as nesting, and a
# value may hold a comma (dns.gardener.cloud/dnsnames=a,b), which both the
# k=v,k=v string and Helm read as the next pair. These render the real edge
# chart and read the annotations it puts on the edge's Service (#1365).
# =============================================================================
# shellcheck disable=SC2016 # the checks are strings the harness evaluates

set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

# shellcheck source=scripts/tests/lib/harness.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/harness.sh"

# shellcheck disable=SC2317,SC2329 # called by the function under test
warn() { printf 'warn: %s\n' "$*" >&2; }
eval "$(sed -n '/^gentian_set_args_from_pairs() {/,/^}/p' scripts/lib/certs.sh)"

# annotations <LB_ANNOTATIONS> — the edge Service's annotations, as JSON.
annotations() {
    local -a args=()
    local arg
    while IFS= read -r arg; do args+=("${arg}"); done < <(gentian_set_args_from_pairs extraAnnotations "$1" 2>/dev/null)
    helm template gentian-edge kernel/manifests/gateway/chart \
        -f kernel/platforms.yaml \
        --set namespace=kernel-edge \
        --set-string platform=openstack \
        --set-string nodeIp=192.0.2.1 \
        "${args[@]+"${args[@]}"}" 2>/dev/null |
        python3 -c '
import json, sys, yaml
for doc in yaml.safe_load_all(sys.stdin):
    if doc and doc.get("kind") == "EnvoyProxy" and doc["metadata"]["name"] == "gentian-edge":
        print(json.dumps(doc["spec"]["provider"]["kubernetes"]["envoyService"]["annotations"]))
'
}

echo ""
echo "lbAnnotations reach the edge Service verbatim"
echo ""

got="$(annotations 'dns.gardener.cloud/class=garden,dns.gardener.cloud/dnsnames=example.org,*.example.org')"
check "a key with dots is one key" \
    '[[ "$(jq -r ".\"dns.gardener.cloud/class\"" <<<"${got}")" == garden ]]' "${got}"
check "a value with a comma keeps both names" \
    '[[ "$(jq -r ".\"dns.gardener.cloud/dnsnames\"" <<<"${got}")" == "example.org,*.example.org" ]]' "${got}"
check "no nested key is left behind" \
    '[[ "$(jq -r "has(\"dns\")" <<<"${got}")" == false ]]' "${got}"
check "the platform's own annotations stay" \
    '[[ "$(jq -r ".\"loadbalancer.openstack.org/keep-floatingip\"" <<<"${got}")" == true ]]' "${got}"

got="$(annotations 'loadbalancer.openstack.org/keep-floatingip=false')"
check "an annotation of the platform's is overridden" \
    '[[ "$(jq -r ".\"loadbalancer.openstack.org/keep-floatingip\"" <<<"${got}")" == false ]]' "${got}"

out="$(gentian_set_args_from_pairs dnsParams 'nameserver=192.0.2.10:53,tsigKeyName=k,zone=example.org' 2>/dev/null | paste -sd' ')"
check "plain pairs are unchanged" \
    '[[ "${out}" == "--set-string dnsParams.nameserver=192.0.2.10:53 --set-string dnsParams.tsigKeyName=k --set-string dnsParams.zone=example.org" ]]' "${out}"

err="$(gentian_set_args_from_pairs extraAnnotations 'stray,a=b' 2>&1 >/dev/null)"
check "a piece before any pair is still refused" '[[ "${err}" == *"malformed extraAnnotations entry: stray"* ]]' "${err}"

finish
