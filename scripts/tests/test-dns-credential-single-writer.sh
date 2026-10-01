#!/usr/bin/env bash
# =============================================================================
# scripts/tests/test-dns-credential-single-writer.sh
# =============================================================================
# cert-manager (C-03, kernel/manifests/cert-manager/chart) and external-dns
# (B-01, kernel/bootstrap/chart) both write the DNS provider's ExternalSecret,
# under the same name, in the same edge namespace. While their specs differed,
# the last apply won: external-dns's version held only its own mapped key, the
# ClusterIssuers lost the key they read, and a production order hung on
# "key not found". Rendered for every provider, as the installer passes them,
# the two must be the same object.
# =============================================================================
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; NC=$'\033[0m'
pass=0; fail=0
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
{ echo "namespaces:"; sed 's/^/  /' kernel/namespaces.yaml; } > "${TMP}/ns.yaml"
EDGE="$(python3 -c "import yaml; print(next(n['name'] for n in yaml.safe_load(open('kernel/namespaces.yaml'))['kernel'] if n['function'] == 'edge'))")"

# The ExternalSecret of the given name from a multi-document render, as
# canonical JSON (empty when absent).
pick() {
    python3 -c '
import sys, yaml, json
for d in yaml.safe_load_all(sys.stdin):
    if d and d.get("kind") == "ExternalSecret" and d["metadata"]["name"] == sys.argv[1]:
        print(json.dumps(d, sort_keys=True))
' "$1"
}

for provider in $(python3 -c "import yaml; print(' '.join(k for k,v in yaml.safe_load(open('kernel/platforms.yaml'))['dnsProviders'].items() if v.get('credential')))"); do
    name="$(python3 -c "import yaml,sys; print(yaml.safe_load(open('kernel/platforms.yaml'))['dnsProviders'][sys.argv[1]]['credential']['secretName'])" "${provider}")"
    # Placeholder params: the guard requires a provider's params, not their values.
    params=()
    while IFS= read -r p; do [[ -n "${p}" ]] && params+=(--set-string "dnsParams.${p}=x"); done < <(
        python3 -c "import yaml,sys; print('\n'.join(yaml.safe_load(open('kernel/platforms.yaml'))['dnsProviders'][sys.argv[1]].get('requiredParams',[])))" "${provider}")

    cm="$(helm template gentian-cert-manager kernel/manifests/cert-manager/chart -f kernel/platforms.yaml \
        -s templates/dns-credentials-externalsecret.yaml \
        --set-string certManagerNamespace="${EDGE}" --set-string kernelDomain=k.example \
        --set-string dnsProvider="${provider}" "${params[@]+"${params[@]}"}" 2>&1 | pick "${name}")"
    bo="$(helm template boot kernel/bootstrap/chart -f "${TMP}/ns.yaml" -f kernel/platforms.yaml \
        --set-string dnsProvider="${provider}" --set-string kernelDomain=k.example --set-string cluster=c \
        --set-string versions.headlamp.chart=0.0.0 --set-string versions.headlamp.repo=https://example.invalid \
        "${params[@]+"${params[@]}"}" 2>&1 | pick "${name}")"

    if [[ -z "${cm}" ]]; then
        printf '  %sFAIL%s  %s: cert-manager chart rendered no ExternalSecret %s\n' "${RED}" "${NC}" "${provider}" "${name}"; fail=$((fail + 1))
    elif [[ -n "${bo}" && "${cm}" != "${bo}" ]]; then
        printf '  %sFAIL%s  %s: the two renders of %s differ\n' "${RED}" "${NC}" "${provider}" "${name}"
        diff <(jq . <<< "${cm}") <(jq . <<< "${bo}") | sed 's/^/        /'
        fail=$((fail + 1))
    else
        printf '  %sok%s    %s: %s\n' "${GREEN}" "${NC}" "${provider}" \
            "$([[ -n "${bo}" ]] && echo "one spec from both charts" || echo "cert-manager only (no external-dns)")"
        pass=$((pass + 1))
    fi
done

echo ""
if (( fail > 0 )); then
    printf '%s%d of %d failed.%s\n' "${RED}" "${fail}" "$((pass + fail))" "${NC}"; exit 1
fi
printf '%sAll %d DNS credential renders agree.%s\n' "${GREEN}" "${pass}" "${NC}"
